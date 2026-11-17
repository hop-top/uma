package discord

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func archiveRequest(r *rig, route model.ExternalRef, undo bool) adapter.ArchiveRequest {
	return adapter.ArchiveRequest{Effect: r.effect(route), Items: []model.ExternalRef{*msgRef(testThread, "1200000000000000001")}, Undo: undo}
}

func TestArchiveThread(t *testing.T) {
	r := newRig(t)
	r.f.thread("acct1", testRoom, testThread)
	route := r.x.threadRef(testRoom, testThread)
	for _, undo := range []bool{false, true} {
		out, err := r.x.Archive(t.Context(), archiveRequest(r, route, undo))
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Archive(undo %v) = %+v, %v", undo, out, err)
		}
		if got := r.acct.channels[testThread].archived; got == undo {
			t.Fatalf("Archive(undo %v): thread archived = %v", undo, got)
		}
		calls := r.callsOf(http.MethodPatch, "channels/"+testThread)
		body := calls[len(calls)-1].args(t)
		if len(body) != 1 || body["archived"] != !undo {
			t.Fatalf("Archive(undo %v) body = %v, want only archived: %v", undo, body, !undo)
		}
		rec, err := r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: r.effect(route), Capability: model.CapabilityArchive, Undo: undo})
		if err != nil || rec.State != model.ProviderAccepted {
			t.Fatalf("Reconcile(undo %v) = %+v, %v", undo, rec, err)
		}
		rec, _ = r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: r.effect(route), Capability: model.CapabilityArchive, Undo: !undo})
		if rec.State != model.Failed || model.CodeOf(rec.Failure) != model.NotFound {
			t.Fatalf("Reconcile of the opposite change = %+v, want not applied", rec)
		}
	}
	// Applying the state already held is applied.
	for range 2 {
		if out, err := r.x.Archive(t.Context(), archiveRequest(r, route, false)); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("repeated Archive = %+v, %v", out, err)
		}
	}
}

func TestArchiveNeedsAThread(t *testing.T) {
	r := newRig(t)
	for name, route := range map[string]model.ExternalRef{"room": r.x.roomRef(testRoom), "user": r.x.userRef(testPerson)} {
		before := len(r.f.calls("acct1"))
		out, err := r.x.Archive(t.Context(), archiveRequest(r, route, false))
		if err != nil || out.State != model.Rejected || model.CodeOf(out.Failure) != model.Unsupported {
			t.Fatalf("%s Archive = %+v, %v, want rejected unsupported", name, out, err)
		}
		if n := len(r.f.calls("acct1")); n != before {
			t.Fatalf("%s Archive made %d calls, want none", name, n-before)
		}
	}
}

func TestArchiveRefusals(t *testing.T) {
	r := newRig(t)
	r.f.thread("acct1", testRoom, testThread)
	route := r.x.threadRef(testRoom, testThread)
	for name, tc := range map[string]struct {
		ans  answer
		want model.ErrorCode
	}{
		"missing permission": {refusal(http.StatusForbidden, codeMissingPermissions), model.Unauthorized},
		"unknown thread":     {refusal(http.StatusNotFound, codeUnknownChannel), model.NotFound},
	} {
		r.f.respond("acct1", tc.ans)
		out, err := r.x.Archive(t.Context(), archiveRequest(r, route, false))
		if err != nil || out.State != model.Rejected || model.CodeOf(out.Failure) != tc.want {
			t.Fatalf("%s: Archive = %+v, %v, want rejected %s", name, out, err, tc.want)
		}
	}
	r.f.respond("acct1", answerLost)
	if _, err := r.x.Archive(t.Context(), archiveRequest(r, route, false)); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Archive with a lost answer = %v, want outcome unknown", err)
	}
	if rec, err := r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: r.effect(route), Capability: model.CapabilityArchive}); err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile after the lost answer = %+v, %v, want applied", rec, err)
	}
}

func TestArchiveCapabilityReportsSharedScope(t *testing.T) {
	r := newRig(t)
	statuses, err := r.x.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []model.Capability
	for _, s := range statuses {
		switch s.Name {
		case model.CapabilityArchive:
			l, err := model.ParseStateLimits(s.Limits)
			if err != nil || !s.Available || l.Scope != model.ScopeShared {
				t.Fatalf("archive status = %+v, limits %+v, %v; want available with shared scope", s, l, err)
			}
			got = append(got, s.Name)
		case model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash:
			t.Fatalf("declared %s: Discord thread archive is the only provider state declared", s.Name)
		}
	}
	if !slices.Contains(got, model.CapabilityArchive) {
		t.Fatal("archive not declared")
	}
}

func TestIdentifyAsksForGuilds(t *testing.T) {
	if testConfig().intents()&intentGuilds == 0 {
		t.Fatal("identify does not ask for GUILDS: THREAD_UPDATE is not dispatched without it")
	}
	if intentGuilds != 1 {
		t.Fatalf("GUILDS intent = %d, want 1<<0", intentGuilds)
	}
}

// threadUpdate is the THREAD_UPDATE dispatch Discord sends when a thread's
// archive state changes: the thread channel object, with the time of the
// change in archive_timestamp.
func threadUpdate(id, parent string, archived bool, at time.Time) map[string]any {
	return map[string]any{
		"id": id, "type": 11, "guild_id": testGuild, "parent_id": parent, "name": "support",
		"thread_metadata": map[string]any{
			"archived": archived, "auto_archive_duration": 1440, "locked": false,
			"archive_timestamp": at.Format("2006-01-02T15:04:05.000000+00:00"),
		},
	}
}

func (r *rig) threadState(t *testing.T, sink *sinkRecorder, d any, seq int64) []model.Envelope {
	t.Helper()
	n := len(sink.envelopes())
	if err := r.handleJob(t, sink, "THREAD_UPDATE", d, seq); err != nil {
		t.Fatalf("THREAD_UPDATE: %v", err)
	}
	return sink.envelopes()[n:]
}

func TestThreadUpdateReportsArchiveState(t *testing.T) {
	r := newRig(t)
	sink := &sinkRecorder{}
	at := testNow.Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		archived bool
		at       time.Time
	}{
		{"archive, by uma or a person or auto-archive", true, at},
		{"unarchive, by a person or by a new message", false, at.Add(time.Minute)},
	} {
		envs := r.threadState(t, sink, threadUpdate(testThread, testRoom, tc.archived, tc.at), 1)
		if len(envs) != 1 {
			t.Fatalf("%s: envelopes = %d, want 1", tc.name, len(envs))
		}
		env := envs[0]
		if env.Kind != model.EnvelopeConversationState || env.State == nil || !env.SystemActor || env.Destination != testApp || env.Assurance != "verified" {
			t.Fatalf("%s: envelope = %+v", tc.name, env)
		}
		if err := env.State.Validate(); err != nil {
			t.Fatalf("%s: report: %v", tc.name, err)
		}
		st := env.State.State
		if env.State.Conversation != r.x.threadRef(testRoom, testThread) || st.Archived != tc.archived || !st.Complete || !st.ObservedAt.Equal(tc.at) || !env.OccurredAt.Equal(tc.at) {
			t.Fatalf("%s: report = %+v, want the thread, archived %v, complete, observed %v", tc.name, env.State, tc.archived, tc.at)
		}
		if st.Trashed || st.Folder != nil || len(st.Labels) > 0 || len(st.Flags) > 0 || st.Read != nil {
			t.Fatalf("%s: report %+v carries state Discord has no thread verb for", tc.name, st)
		}
	}
	a, b := sink.envelopes()[0], sink.envelopes()[1]
	if a.EventID == b.EventID || a.EventID == "" {
		t.Fatalf("event IDs %q and %q: each change is an event of its own", a.EventID, b.EventID)
	}
	// A redelivery of a dispatch is the same event.
	again := r.threadState(t, sink, threadUpdate(testThread, testRoom, true, at), 2)
	if len(again) != 1 || again[0].EventID != a.EventID {
		t.Fatalf("redelivered dispatch = %+v, want event %q", again, a.EventID)
	}
}

func TestThreadUpdateIgnored(t *testing.T) {
	r := newRig(t)
	sink := &sinkRecorder{}
	for name, d := range map[string]map[string]any{
		"a channel that is not a thread": {"id": testRoom, "type": 0, "guild_id": testGuild, "name": "general"},
		"a thread with no metadata":      {"id": testThread, "type": 11, "parent_id": testRoom, "guild_id": testGuild},
		"a thread with no parent":        {"id": testThread, "type": 11, "thread_metadata": map[string]any{"archived": true}},
		"a thread that is no snowflake":  threadUpdate("not-a-thread", testRoom, true, testNow),
	} {
		if envs := r.threadState(t, sink, d, 1); len(envs) != 0 {
			t.Fatalf("%s: envelopes = %+v, want none", name, envs)
		}
	}
}

// A report of a conversation uma does not hold is the sink's NotFound; it
// is not a failure of the dispatch.
func TestThreadUpdateOfUnknownConversationIsDropped(t *testing.T) {
	r := newRig(t)
	sink := &sinkRecorder{fail: func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeConversationState {
			return model.NewError(model.NotFound, "unknown conversation")
		}
		return nil
	}}
	if err := r.handleJob(t, sink, "THREAD_UPDATE", threadUpdate(testThread, testRoom, true, testNow), 1); err != nil {
		t.Fatalf("THREAD_UPDATE of an unknown conversation = %v, want it dropped", err)
	}
}

// THREAD_UPDATE reaches the receiver over the gateway, in the thread's lane
// with the thread's messages.
func TestGatewayDispatchesThreadUpdate(t *testing.T) {
	r := gatewayRig(t)
	h := r.watch()
	sink := &sinkRecorder{}
	r.run(t, sink)
	c := r.acct.gw.nextConn(t)
	seq := c.dispatch("THREAD_UPDATE", threadUpdate(testThread, testRoom, true, testNow))
	h.wait(t, seq)
	envs := sink.envelopes()
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeConversationState || !envs[0].State.State.Archived {
		t.Fatalf("recorded %+v, want the archive report", envs)
	}
	if got := laneKey("THREAD_UPDATE", mustJSON(threadUpdate(testThread, testRoom, true, testNow))); got != laneKey("MESSAGE_CREATE", mustJSON(map[string]any{"channel_id": testThread})) {
		t.Fatalf("lane %q: a thread's state changes are ordered with its messages", got)
	}
}
