package slack

import (
	"net/http"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) archive(t *testing.T, route model.ExternalRef, undo bool) (adapter.Outcome, error) {
	t.Helper()
	return r.x.Archive(t.Context(), adapter.ArchiveRequest{Effect: r.effect(route), Undo: undo})
}

func TestArchiveAndUnarchiveTheChannel(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")

	out, err := r.archive(t, room, false)
	wantAccepted(t, out, err)
	calls := r.callsOf("conversations.archive")
	if len(calls) != 1 || !calls[0].Applied || calls[0].args(t)["channel"] != testRoom {
		t.Fatalf("calls = %+v, want one applied conversations.archive of the channel", calls)
	}
	if !r.f.archivedChannel("acct1", testRoom) {
		t.Fatal("channel not archived")
	}

	out, err = r.archive(t, room, true)
	wantAccepted(t, out, err)
	if calls := r.callsOf("conversations.unarchive"); len(calls) != 1 || calls[0].args(t)["channel"] != testRoom || r.f.archivedChannel("acct1", testRoom) {
		t.Fatalf("calls = %+v, archived = %v, want one conversations.unarchive and an open channel", calls, r.f.archivedChannel("acct1", testRoom))
	}
}

// Slack answers already_archived and not_archived for the state the channel
// holds: that is what was asked.
func TestArchiveOfTheStateHeldIsApplied(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")
	out, err := r.archive(t, room, true)
	wantAccepted(t, out, err)
	r.f.setArchived("acct1", testRoom, true)
	out, err = r.archive(t, room, false)
	wantAccepted(t, out, err)
}

func TestArchiveRefusesThreadAndDirect(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")
	for name, route := range map[string]model.ExternalRef{
		"thread": r.x.threadRef(testRoom, testTS),
		"direct": r.x.userRef(testPerson),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := r.archive(t, route, false)
			wantRejected(t, out, err, model.Unsupported)
		})
	}
	if got := len(r.f.calls("acct1")); got != 0 {
		t.Fatalf("%d calls, want none: a thread or a direct message is no archivable object", got)
	}
	bad := model.ExternalRef{Channel: testChannel, Namespace: Namespace, Kind: "unknown", ID: testRoom}
	out, err := r.archive(t, bad, false)
	wantError(t, out, err, model.Validation)
}

func TestArchiveRefusals(t *testing.T) {
	for _, tc := range []struct {
		code string
		want model.ErrorCode
	}{
		{"cant_archive_general", model.Unsupported},
		{"method_not_supported_for_channel_type", model.Unsupported},
		{"restricted_action", model.Unauthorized},
		{"missing_scope", model.Unauthorized},
		{"channel_not_found", model.NotFound},
	} {
		t.Run(tc.code, func(t *testing.T) {
			r := newRig(t)
			r.f.put("acct1", testRoom, testPerson, "hello", "", "")
			r.f.respond("acct1", failWith(tc.code))
			out, err := r.archive(t, r.x.roomRef(testRoom), false)
			wantRejected(t, out, err, tc.want)
		})
	}
	r := newRig(t)
	r.f.respond("acct1", failWith("not_authed"))
	out, err := r.archive(t, r.x.roomRef(testRoom), false)
	wantError(t, out, err, model.Unauthorized)
}

// A message posted to an archived channel is refused as Slack does.
func TestArchivedChannelRefusesSends(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")
	r.f.setArchived("acct1", testRoom, true)
	out, err := r.send(t, r.x.roomRef(testRoom), text("late"))
	wantRejected(t, out, err, model.Unauthorized)
}

func TestArchiveAnswerLostIsReconciled(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")
	r.f.respond("acct1", answerLost)
	if out, err := r.archive(t, room, false); err == nil {
		t.Fatalf("archive = %+v, want a lost answer", out)
	}
	req := adapter.ReconcileRequest{Effect: r.effect(room), Capability: model.CapabilityArchive}
	out, err := r.reconcile(t, req)
	wantAccepted(t, out, err)
	if calls := r.callsOf("conversations.archive"); len(calls) != 1 {
		t.Fatalf("%d archive calls, want the one lost: reconcile never writes", len(calls))
	}

	// The request never reached Slack: the channel is open.
	r2 := newRig(t)
	r2.f.put("acct1", testRoom, testPerson, "hello", "", "")
	r2.f.respond("acct1", requestLost)
	_, _ = r2.archive(t, room, false)
	out, err = r2.reconcile(t, req)
	notFound(t, out, err)

	// An unarchive that applied shows an open channel.
	r2.f.setArchived("acct1", testRoom, false)
	req.Undo = true
	out, err = r2.reconcile(t, req)
	wantAccepted(t, out, err)
}

func TestCapabilitiesReportArchiveAsShared(t *testing.T) {
	x := open(t, testConfig())
	statuses, err := x.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Name != model.CapabilityArchive {
			continue
		}
		l, err := model.ParseStateLimits(s.Limits)
		if err != nil || l.Scope != model.ScopeShared || !s.Available {
			t.Fatalf("archive status = %+v, limits %+v, %v; want available with scope shared", s, l, err)
		}
		return
	}
	t.Fatal("archive not reported")
}

func TestDeclaresArchiveAlone(t *testing.T) {
	d := NewFactory(Options{}).Describe()
	declared := map[model.Capability]bool{}
	for _, c := range d.Capabilities {
		declared[c] = true
	}
	if !declared[model.CapabilityArchive] {
		t.Fatal("archive not declared")
	}
	for _, c := range []model.Capability{model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash} {
		if declared[c] {
			t.Errorf("%s declared: a bot token cannot, or Slack has nothing for it", c)
		}
	}
	for _, p := range []string{"channels:manage", "groups:write", "channels:read", "groups:read"} {
		found := false
		for _, have := range d.RequiredPermissions {
			found = found || have == p
		}
		if !found {
			t.Errorf("permission %s missing", p)
		}
	}
}

// channel_archive and channel_unarchive, and group_archive and
// group_unarchive for a private channel, carry the channel and who changed
// it; https://docs.slack.dev/reference/events/channel_archive/.
func TestArchiveEventsReportTheChannelState(t *testing.T) {
	for typ, archived := range map[string]bool{"channel_archive": true, "channel_unarchive": false, "group_archive": true, "group_unarchive": false} {
		t.Run(typ, func(t *testing.T) {
			x := open(t, testConfig())
			s := &sinkRecorder{}
			ev := map[string]any{"type": typ, "channel": testRoom, "user": testPerson, "event_ts": testTS}
			if res := serve(t, x, s, jsonRequest(callback("Ev9", ev))); res.Code != http.StatusOK {
				t.Fatalf("answer = %d, want 200", res.Code)
			}
			e := wantOne(t, s)
			if e.Kind != model.EnvelopeConversationState || e.EventID != "Ev9" || e.Destination != testTeam || !e.SystemActor || e.Assurance != "verified" {
				t.Fatalf("envelope = %+v, want a verified system conversation.state of the team", e)
			}
			if !e.Sender.IsZero() || e.MessageID != "" || len(e.References) != 0 || len(e.Content.Parts) != 0 {
				t.Errorf("envelope = %+v, want no sender, message, references or content", e)
			}
			st := e.State
			if st == nil || st.Conversation != x.roomRef(testRoom) || st.State.Archived != archived || !st.State.Complete || !st.State.ObservedAt.Equal(slackTime(testTS)) {
				t.Fatalf("state = %+v, want the room, archived=%v, complete, observed at the event", st, archived)
			}
			if err := e.State.Validate(); err != nil {
				t.Errorf("state invalid: %v", err)
			}
		})
	}
}

func TestArchiveEventRefusals(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{}
	bad := []map[string]any{
		{"type": "channel_archive", "channel": "general", "event_ts": testTS},
		{"type": "channel_archive", "channel": testRoom},
		{"type": "channel_unarchive", "channel": testRoom, "event_ts": "now"},
		{"type": "group_archive", "channel": testRoom},
		{"type": "group_unarchive", "channel": "general", "event_ts": testTS},
	}
	for _, ev := range bad {
		if res := serve(t, x, s, jsonRequest(callback("Ev9", ev))); res.Code != http.StatusBadRequest {
			t.Errorf("%+v answered %d, want 400", ev, res.Code)
		}
	}
	wantNone(t, s)
}
