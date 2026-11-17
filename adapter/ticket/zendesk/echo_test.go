package zendesk_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// seedTicket gives ticket 100 a conversation through an inbound comment.
func seedTicket(t *testing.T, h *host) model.Conversation {
	t.Helper()
	if code := h.post(commentEvent(t, "evt-seed", "100", "c-seed", "42", false, ptr(true), "My printer is on fire"), webhookSecret); code != http.StatusOK {
		t.Fatalf("seed comment: status %d", code)
	}
	return h.conversation("100")
}

// commentOf returns the comment ID and audit of the newest audit of
// ticket 100.
func commentOf(t *testing.T, h *host) (string, fakeAudit) {
	t.Helper()
	au := h.fake.lastAudit(h.acct, "100")
	for _, e := range au.Events {
		if e["type"] == "Comment" {
			return e["id"].(string), au
		}
	}
	t.Fatalf("audit %s holds no comment", au.ID)
	return "", au
}

// A reply's echo that arrives before the send's answer joins the reply:
// no second message, and the lost answer settles from the echo without a
// second write.
func TestReplyEchoBeforeAnswerIsNotANewMessage(t *testing.T) {
	h := newHost(t)
	conv := seedTicket(t, h)
	h.acct.script = []conformance.Response{{Outcome: conformance.OutcomeAnswerLost}}
	ref := h.reply(conv.Ref, "op-reply", "We are on it")
	h.work()
	if op := h.op(ref); op.State != model.Unknown {
		t.Fatalf("operation after a lost answer = %+v, want outcome_unknown", op)
	}
	id, au := commentOf(t, h)
	if au.Metadata["custom"] == nil {
		t.Fatal("the reply carries no echo marker")
	}
	before := len(h.messages())
	if code := h.post(commentEvent(t, "evt-echo", "100", id, "777", true, ptr(true), "We are on it"), webhookSecret); code != http.StatusOK {
		t.Fatalf("echo webhook: status %d", code)
	}
	msgs := h.messages()
	if len(msgs) != before {
		t.Fatalf("messages = %d after the reply's echo, want %d: an own comment came back as a new message", len(msgs), before)
	}
	var outbound *model.Message
	for i := range msgs {
		if msgs[i].Origin != nil {
			outbound = &msgs[i]
		}
	}
	if outbound == nil || !slices.Contains(outbound.External, model.ProviderMessageRef(hostChannel, id)) {
		t.Fatalf("outbound message %+v does not hold the echoed comment ID %s", outbound, id)
	}
	h.clock.Advance(time.Hour)
	h.work()
	if op := h.op(ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation after its echo = %+v, want provider_accepted", op)
	}
	if n := h.fake.writesOf(h.acct); n != 1 {
		t.Fatalf("ticket updates = %d, want 1: the reply must never be written twice", n)
	}
}

// A reply's echo after its answer is recognized by the comment ID alone.
func TestReplyEchoAfterAnswer(t *testing.T) {
	h := newHost(t)
	conv := seedTicket(t, h)
	ref := h.reply(conv.Ref, "op-reply", "We are on it")
	h.work()
	op := h.op(ref)
	if op.State != model.ProviderAccepted {
		t.Fatalf("operation = %+v, want provider_accepted", op)
	}
	id, au := commentOf(t, h)
	want := []model.ExternalRef{
		model.ProviderMessageRef(hostChannel, id),
		{Channel: hostChannel, Namespace: zendesk.Namespace, Kind: zendesk.RefKindAudit, ID: au.ID, ParentID: "100"},
	}
	if !slices.Equal(op.ProviderReferences, want) {
		t.Fatalf("operation references = %v, want %v", op.ProviderReferences, want)
	}
	before := len(h.messages())
	if code := h.post(commentEvent(t, "evt-echo", "100", id, "777", true, ptr(true), "We are on it"), webhookSecret); code != http.StatusOK {
		t.Fatalf("echo webhook: status %d", code)
	}
	if n := len(h.messages()); n != before {
		t.Fatalf("messages = %d after the reply's echo, want %d", n, before)
	}
}

func internalNote(body string) model.Extension {
	data, _ := json.Marshal(map[string]any{"body": body})
	return model.Extension{Name: zendesk.ExtensionInternalNote, Version: zendesk.ExtensionVersion, Data: data}
}

var staff = model.Audience{Visibility: model.Internal, PolicyRef: "staff"}

// An internal note goes out private, under a non-public audience only, and
// its echo, by webhook or by poll, never becomes a message.
func TestInternalNote(t *testing.T) {
	h := newHost(t)
	conv := seedTicket(t, h)

	for i, bad := range []struct {
		name   string
		aud    model.Audience
		action model.Extension
	}{
		{"public audience", model.Audience{Visibility: model.Public}, internalNote("leak")},
		{"public flag", staff, model.Extension{Name: zendesk.ExtensionInternalNote, Version: 1, Data: json.RawMessage(`{"body":"leak","public":true}`)}},
		{"no body", staff, model.Extension{Name: zendesk.ExtensionInternalNote, Version: 1, Data: json.RawMessage(`{}`)}},
	} {
		id := model.OperationID("op-bad-" + string(rune('a'+i)))
		if _, err := h.execute(conv.Ref, id, bad.aud, bad.action); !model.IsCode(err, model.Validation) {
			t.Fatalf("%s: Execute = %v, want %s at the call", bad.name, err, model.Validation)
		}
	}
	h.work()
	if n := h.fake.writesOf(h.acct); n != 0 {
		t.Fatalf("ticket updates = %d after refused notes, want 0", n)
	}

	a, err := h.execute(conv.Ref, "op-note", staff, internalNote("Customer is a VIP"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	h.work()
	if op := h.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("note operation = %+v, want provider_accepted", op)
	}
	calls := h.fake.callsOf(h.acct)
	last := calls[len(calls)-1]
	if last.Audience != staff || last.Action == nil || last.Action.Name != zendesk.ExtensionInternalNote {
		t.Fatalf("provider call = %+v, want the note under the staff audience", last)
	}
	id, au := commentOf(t, h)
	for _, e := range au.Events {
		if e["type"] == "Comment" && e["public"] != false {
			t.Fatalf("note comment public = %v, want false", e["public"])
		}
	}
	before := len(h.messages())
	if code := h.post(commentEvent(t, "evt-note", "100", id, "777", true, ptr(false), "Customer is a VIP"), webhookSecret); code != http.StatusOK {
		t.Fatalf("note echo webhook: status %d", code)
	}
	if n := len(h.messages()); n != before {
		t.Fatalf("messages = %d after the note's webhook echo, want %d", n, before)
	}
	// The same echo read by the poller.
	marker := au.Metadata["custom"].(map[string]any)
	h.fake.addComment(h.acct, "poll-note", "100", hostBrand, id, "777", "Customer is a VIP", ptr(false), true, marker)
	h.receiveOnce()
	if n := len(h.messages()); n != before {
		t.Fatalf("messages = %d after the note's polled echo, want %d", n, before)
	}
}

// A ticket update passes its fields through unchanged and refuses the
// fields replies and notes own.
func TestTicketUpdate(t *testing.T) {
	h := newHost(t)
	conv := seedTicket(t, h)
	for i, data := range []string{`{"comment":{"body":"x","public":true}}`, `{"metadata":{"a":1}}`, `{}`} {
		id := model.OperationID("op-bad-" + string(rune('a'+i)))
		_, err := h.execute(conv.Ref, id, staff, model.Extension{Name: zendesk.ExtensionUpdate, Version: 1, Data: json.RawMessage(data)})
		if !model.IsCode(err, model.Validation) {
			t.Fatalf("Execute %s = %v, want %s at the call", data, err, model.Validation)
		}
	}
	h.work()
	if n := h.fake.writesOf(h.acct); n != 0 {
		t.Fatalf("ticket updates = %d after refused updates, want 0", n)
	}
	a, err := h.execute(conv.Ref, "op-update", staff, model.Extension{Name: zendesk.ExtensionUpdate, Version: 1, Data: json.RawMessage(`{"status":"solved","tags":["vip"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	h.work()
	if op := h.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("update operation = %+v, want provider_accepted", op)
	}
	calls := h.fake.callsOf(h.acct)
	if n := h.fake.writesOf(h.acct); n != 1 || !sameJSON(calls[len(calls)-1].Action.Data, `{"status":"solved","tags":["vip"]}`) {
		t.Fatalf("writes %d, last action %s; want one update with the fields as given", n, calls[len(calls)-1].Action.Data)
	}
}

func sameJSON(a []byte, b string) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal([]byte(b), &y) == nil && jsonEqual(x, y)
}

func jsonEqual(x, y any) bool {
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(bx) == string(by)
}

// optInNotes turns on the host inbox's delivery of internal notes.
func optInNotes(t *testing.T, h *host) {
	t.Helper()
	var in model.Inbox
	h.view(func(r store.Reader) (err error) {
		in, err = r.GetInbox(h.ctx, hostInbox)
		return err
	})
	in.DeliverNotes = true
	if _, err := h.client.Configuration().ConfigureInbox(h.ctx, model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "c-cfg"}, Operation: "cfg-notes", ExpectedRevision: in.Revision},
		Inbox:   in,
	}); err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
}

// In an inbox that opted in, AddInternalNote delivers the note as a
// private comment through the note capability. Its echo joins the note,
// even ahead of a lost answer: the note takes the comment ID at once and
// the operation settles from the echo without a second write.
func TestDeliveredInternalNote(t *testing.T) {
	h := newHost(t)
	optInNotes(t, h)
	conv := seedTicket(t, h)
	h.acct.script = []conformance.Response{{Outcome: conformance.OutcomeAnswerLost}}
	a, err := h.client.Messages().AddInternalNote(h.ctx, model.InternalNote{
		Command: model.Command{Call: model.Call{Correlation: "c-note"}, Operation: "op-note"}, Conversation: conv.Ref,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Customer is a VIP"}}}, PolicyRef: "staff",
	})
	if err != nil {
		t.Fatalf("AddInternalNote: %v", err)
	}
	if a.Operation.State != model.Queued {
		t.Fatalf("note operation = %+v, want queued for delivery", a.Operation)
	}
	h.work()
	if op := h.op(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("note operation after a lost answer = %+v, want outcome_unknown", op)
	}
	calls := h.fake.callsOf(h.acct)
	last := calls[len(calls)-1]
	if last.Capability != model.CapabilityInternalNote || last.Audience != staff || len(last.Content.Parts) != 1 {
		t.Fatalf("provider call = %+v, want the note under the staff audience", last)
	}
	id, au := commentOf(t, h)
	for _, e := range au.Events {
		if e["type"] == "Comment" && e["public"] != false {
			t.Fatalf("note comment public = %v, want false", e["public"])
		}
	}
	before := len(h.messages())
	if code := h.post(commentEvent(t, "evt-note", "100", id, "777", true, ptr(false), "Customer is a VIP"), webhookSecret); code != http.StatusOK {
		t.Fatalf("note echo webhook: status %d", code)
	}
	msgs := h.messages()
	if len(msgs) != before {
		t.Fatalf("messages = %d after the note's echo, want %d", len(msgs), before)
	}
	for _, m := range msgs {
		if m.Ref == *a.Operation.ResultMessage && !slices.Contains(m.External, model.ProviderMessageRef(hostChannel, id)) {
			t.Fatalf("note %+v does not hold its echoed comment ID %s", m, id)
		}
	}
	h.clock.Advance(time.Hour)
	h.work()
	if op := h.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("note operation after its echo = %+v, want provider_accepted", op)
	}
	if n := h.fake.writesOf(h.acct); n != 1 {
		t.Fatalf("ticket updates = %d, want 1", n)
	}
}

// A public audience never reaches Zendesk as a note.
func TestSendNoteRefusesPublic(t *testing.T) {
	called := false
	inst := openAgainst(t, func(http.ResponseWriter, *http.Request) { called = true }, nil)
	req := sendReq()
	out, err := inst.SendNote(t.Context(), adapter.NoteRequest{Effect: req.Effect, Content: req.Content})
	if err != nil || out.State != model.Rejected || called {
		t.Fatalf("SendNote = %+v, %v (called %v); want rejected without a call", out, err, called)
	}
}
