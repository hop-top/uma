package slack

import (
	"strconv"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) reconcile(t *testing.T, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t.Helper()
	if req.AttemptedAt.IsZero() {
		req.AttemptedAt = testNow
	}
	return r.x.Reconcile(t.Context(), req)
}

func notFound(t *testing.T, out adapter.Outcome, err error) {
	t.Helper()
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound || out.Failure.Retry != model.NoRetry {
		t.Fatalf("outcome = %+v, %v; want failed not_found: nothing applied, a retry is safe", out, err)
	}
}

func sendReq(r *rig, route model.ExternalRef, c model.Content) adapter.ReconcileRequest {
	return adapter.ReconcileRequest{Effect: r.effect(route), Capability: model.CapabilitySend, Content: c}
}

func TestReconcileFindsASentMessageByItsAttempt(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.respond("acct1", answerLost)
	if out, err := r.send(t, room, text("hello")); err == nil {
		t.Fatalf("send = %+v, want a lost answer", out)
	}
	// Other messages, even with the same text, are not the attempt's.
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")
	out, err := r.reconcile(t, sendReq(r, room, text("hello")))
	wantAccepted(t, out, err)
	sent := r.f.messages("acct1", testRoom)
	if len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(testChannel, testRoom+":"+sent[0].ts) {
		t.Fatalf("references = %+v, want the sent message %s", out.References, sent[0].ts)
	}
	for _, c := range r.f.calls("acct1")[1:] {
		if c.Method == "chat.postMessage" {
			t.Fatalf("reconcile sent a message: %+v", c)
		}
	}
	if q := r.callsOf("conversations.history")[0].args(t); q["include_all_metadata"] != true || q["channel"] != testRoom {
		t.Errorf("history arguments = %+v, want the conversation with metadata", q)
	}
}

func TestReconcileOfAMessageThatNeverLanded(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.respond("acct1", requestLost)
	_, _ = r.send(t, room, text("hello"))
	r.f.put("acct1", testRoom, testPerson, "hello", "", "")
	out, err := r.reconcile(t, sendReq(r, room, text("hello")))
	notFound(t, out, err)
}

// Without metadata (Slack refused the schema) the attempt is found by what
// it sent: the bot's own message, after the attempt, with the text.
func TestReconcileFindsAMessageWithoutMetadataByItsText(t *testing.T) {
	r := newRig(t)
	r.x.noMetadata.Store(true)
	room := r.x.roomRef(testRoom)
	r.f.put("acct1", testRoom, testBotUser, "hello", "1600000000.000001", "") // before the attempt
	other := r.f.put("acct1", testRoom, testBotUser, "something else", "", "")
	other.botID = testBotID
	r.f.respond("acct1", answerLost)
	_, _ = r.send(t, room, text("hello <b>"))
	out, err := r.reconcile(t, sendReq(r, room, text("hello <b>")))
	wantAccepted(t, out, err)
	if len(out.References) != 1 || out.References[0].ID == testRoom+":1600000000.000001" {
		t.Fatalf("references = %+v, want the message sent after the attempt", out.References)
	}
	// A message by someone else with the same text is not the bot's.
	r2 := newRig(t)
	r2.x.noMetadata.Store(true)
	r2.f.put("acct1", testRoom, testPerson, "hello", "", "")
	out, err = r2.reconcile(t, sendReq(r2, room, text("hello")))
	notFound(t, out, err)
}

// A message that carries the marker of another attempt is that attempt's,
// whatever its text.
func TestReconcileDoesNotAdoptAnotherAttemptsMessage(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	m := r.f.put("acct1", testRoom, testBotUser, "same words", "", "")
	m.meta = &fakeMeta{EventType: MetadataEventType, EventPayload: map[string]any{"operation": string(testOp), "attempt": "00000ad0-0000-7000-8000-0000000000f9"}}
	out, err := r.reconcile(t, sendReq(r, room, text("same words")))
	notFound(t, out, err)
}

func TestReconcileInAThread(t *testing.T) {
	parent := "1699999999.000100"
	for name, tc := range map[string]struct {
		route func(*Instance) model.ExternalRef
		mod   func(*adapter.ReconcileRequest)
	}{
		"a thread route": {func(x *Instance) model.ExternalRef { return x.threadRef(testRoom, parent) }, nil},
		"an answer to a message": {func(x *Instance) model.ExternalRef { return x.roomRef(testRoom) }, func(q *adapter.ReconcileRequest) {
			ref := model.ProviderMessageRef(testChannel, testRoom+":"+parent)
			q.Target = &ref
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.put("acct1", testRoom, testPerson, "parent", parent, "")
			req := adapter.SendRequest{Effect: r.effect(tc.route(r.x)), Content: text("in thread")}
			if tc.mod != nil {
				q := adapter.ReconcileRequest{Effect: req.Effect}
				tc.mod(&q)
				req.Target = q.Target
			}
			r.f.respond("acct1", answerLost)
			if _, err := r.x.Send(t.Context(), req); err == nil {
				t.Fatal("send did not lose its answer")
			}
			rq := adapter.ReconcileRequest{Effect: req.Effect, Capability: model.CapabilitySend, Content: text("in thread")}
			out, err := r.reconcile(t, rq)
			wantAccepted(t, out, err)
			if got := r.callsOf("conversations.replies"); len(got) == 0 || got[0].args(t)["ts"] != parent {
				t.Fatalf("replies calls = %+v, want the thread read", got)
			}
		})
	}
}

func TestReconcileOfADirectMessage(t *testing.T) {
	r := newRig(t)
	route := r.x.userRef(testPerson)
	r.f.respond("acct1", answerLost)
	if _, err := r.send(t, route, text("psst")); err == nil {
		t.Fatal("send did not lose its answer")
	}
	out, err := r.reconcile(t, sendReq(r, route, text("psst")))
	wantAccepted(t, out, err)
	if len(out.References) != 1 || out.References[0].ID[:5] != "D0DM1" {
		t.Fatalf("references = %+v, want the direct message", out.References)
	}
}

func TestReconcileReadsPagesAndAdmitsWhenItCannotFinish(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.respond("acct1", answerLost)
	_, _ = r.send(t, room, text("hello"))
	// More messages after the attempt than a page: the attempt's is on the
	// last page, oldest.
	for i := range 250 {
		r.f.put("acct1", testRoom, testPerson, "noise "+strconv.Itoa(i), "", "")
	}
	out, err := r.reconcile(t, sendReq(r, room, text("hello")))
	wantAccepted(t, out, err)
	if n := len(r.callsOf("conversations.history")); n != 2 {
		t.Fatalf("history calls = %d, want two pages", n)
	}

	// A listing cut short settles nothing: neither found nor not found.
	r2 := newRig(t)
	r2.f.respond("acct1", requestLost)
	_, _ = r2.send(t, room, text("hello"))
	for i := range reconcilePages*historyPage + 1 {
		r2.f.put("acct1", testRoom, testPerson, "noise "+strconv.Itoa(i), "", "")
	}
	out, err = r2.reconcile(t, sendReq(r2, room, text("hello")))
	if err != nil || out.State != model.Unknown {
		t.Fatalf("outcome = %+v, %v; want unknown: the window was not read to its end", out, err)
	}
}

func TestReconcileReadFailures(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.acct.rateLimitedHistory = true
	out, err := r.reconcile(t, sendReq(r, room, text("x")))
	wantError(t, out, err, model.RateLimited)

	r2 := newRig(t)
	out, err = r2.reconcile(t, sendReq(r2, r2.x.roomRef("C0NOPE"), text("x")))
	// A conversation that is not there is an error of the read, never "the
	// attempt did not land": that would invite a resend.
	wantError(t, out, err, model.NotFound)

	_ = r2.x.Close(t.Context())
	out, err = r2.reconcile(t, sendReq(r2, room, text("x")))
	wantError(t, out, err, model.Unavailable)

	r3 := newRig(t)
	out, err = r3.reconcile(t, adapter.ReconcileRequest{Effect: r3.effect(room), Capability: model.CapabilityRespond})
	wantError(t, out, err, model.Unsupported)
}

func TestReconcileEdit(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	ref, m := r.own(testRoom, "before")
	e := r.effect(room)
	e.Target = &ref
	req := adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityEdit, Content: text("after")}

	out, err := r.reconcile(t, req)
	notFound(t, out, err)

	// Applied: the message carries the attempt's marker, or its text.
	m.meta = &fakeMeta{EventType: MetadataEventType, EventPayload: map[string]any{"operation": string(testOp), "attempt": string(testAttempt)}}
	m.text = "elsewhere"
	out, err = r.reconcile(t, req)
	wantAccepted(t, out, err)
	m.meta = nil
	m.text = "after"
	out, err = r.reconcile(t, req)
	wantAccepted(t, out, err)

	// The text of an edit that carried characters Slack encodes.
	req.Content = text("a < b")
	m.text = "a &lt; b"
	out, err = r.reconcile(t, req)
	wantAccepted(t, out, err)

	// Gone: the target is no longer Slack's to edit.
	r.f.accounts["acct1"].convs[testRoom].msgs = nil
	out, err = r.reconcile(t, req)
	wantRejected(t, out, err, model.NotFound)
}

func TestReconcileEditOfAReply(t *testing.T) {
	r := newRig(t)
	parent := "1699999999.000100"
	r.f.put("acct1", testRoom, testPerson, "parent", parent, "")
	reply := r.f.put("acct1", testRoom, testBotUser, "reply after", "", parent)
	ref := model.ProviderMessageRef(testChannel, messageID(testRoom, reply.ts))
	e := r.effect(r.x.roomRef(testRoom))
	e.Target = &ref
	out, err := r.reconcile(t, adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityEdit, Content: text("reply after")})
	wantAccepted(t, out, err)
}

func TestReconcileDelete(t *testing.T) {
	r := newRig(t)
	ref, m := r.own(testRoom, "x")
	e := r.effect(r.x.roomRef(testRoom))
	e.Target = &ref
	req := adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityDelete}
	out, err := r.reconcile(t, req)
	notFound(t, out, err)
	r.f.accounts["acct1"].convs[testRoom].msgs = nil
	_ = m
	out, err = r.reconcile(t, req)
	wantAccepted(t, out, err)
}

func TestReconcileReact(t *testing.T) {
	r := newRig(t)
	ref, m := r.own(testRoom, "x")
	e := r.effect(r.x.roomRef(testRoom))
	e.Target = &ref
	add := adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityReact, Emoji: "👍"}
	remove := add
	remove.Remove = true

	out, err := r.reconcile(t, add)
	notFound(t, out, err)
	out, err = r.reconcile(t, remove)
	wantAccepted(t, out, err)

	m.reactions = map[string][]string{"thumbsup": {testPerson}}
	out, err = r.reconcile(t, add)
	notFound(t, out, err) // someone else's reaction is not the bot's
	m.reactions = map[string][]string{"thumbsup": {testPerson, testBotUser}}
	out, err = r.reconcile(t, add)
	wantAccepted(t, out, err)
	out, err = r.reconcile(t, remove)
	notFound(t, out, err)

	if q := r.callsOf("reactions.get")[0].args(t); q["full"] != true || q["timestamp"] != m.ts {
		t.Errorf("reactions.get arguments = %+v, want the full list of the message", q)
	}
	add.Emoji = "🦄"
	out, err = r.reconcile(t, add)
	wantRejected(t, out, err, model.Unsupported)
	r.f.accounts["acct1"].convs[testRoom].msgs = nil
	add.Emoji = "👍"
	out, err = r.reconcile(t, add)
	wantRejected(t, out, err, model.NotFound)
}

var _ = time.Second

func TestReconcileTrustsNoBoundButItsOwn(t *testing.T) {
	r := newRig(t)
	r.x.noMetadata.Store(true)
	r.acct.ignoreBounds = true
	room := r.x.roomRef(testRoom)
	r.f.put("acct1", testRoom, testBotUser, "hello", "1600000000.000001", "")
	out, err := r.reconcile(t, sendReq(r, room, text("hello")))
	notFound(t, out, err)
}

func TestReconcileAdoptsTheEarliestOfIdenticalMessages(t *testing.T) {
	r := newRig(t)
	r.x.noMetadata.Store(true)
	room := r.x.roomRef(testRoom)
	first := r.f.put("acct1", testRoom, testBotUser, "ok", "", "")
	r.f.put("acct1", testRoom, testBotUser, "ok", "", "")
	r.f.put("acct1", testRoom, testBotUser, "ok", "", "")
	out, err := r.reconcile(t, sendReq(r, room, text("ok")))
	wantAccepted(t, out, err)
	if out.References[0].ID != testRoom+":"+first.ts {
		t.Fatalf("references = %+v, want the earliest, %s", out.References, first.ts)
	}
}

func TestReconcileComparesBlocksPresence(t *testing.T) {
	blocks := model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "hello"},
		{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(`[{"type":"divider"}]`)}},
	}}
	room := func(r *rig) model.ExternalRef { return r.x.roomRef(testRoom) }

	r := newRig(t)
	r.x.noMetadata.Store(true)
	r.f.put("acct1", testRoom, testBotUser, "hello", "", "") // no blocks
	out, err := r.reconcile(t, sendReq(r, room(r), blocks))
	notFound(t, out, err)

	r2 := newRig(t)
	r2.x.noMetadata.Store(true)
	m := r2.f.put("acct1", testRoom, testBotUser, "hello", "", "")
	m.blocks = []byte(`[{"type":"divider","block_id":"b1"}]`) // Slack adds block IDs
	out, err = r2.reconcile(t, sendReq(r2, room(r2), blocks))
	wantAccepted(t, out, err)
	out, err = r2.reconcile(t, sendReq(r2, room(r2), text("hello")))
	notFound(t, out, err)
}
