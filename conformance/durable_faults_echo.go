package conformance

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// The provider applies a send and shows the sent message back inbound,
// without an echo reference, before the send's answer reaches the engine:
// the copy is recorded as an inbound message. The process then dies
// before the operation settles. The next owner settles from the recorded
// answer, which names the copy's provider message ID: the copy is linked
// to the outbound message as its echo, with a message.updated event, and
// nothing is deleted.
func echoBeforeSendAnswerIsLinked(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	sending := f.gate("provider send")
	f.p.respond(held(sending))
	p := f.start(t)
	_, stop := p.receive()
	op := p.reply(t, "op-1")
	p.work()
	sending.await(t)
	sent := f.p.lastSend()
	echo := f.message("evt-echo", f.p.ref(sent).ID)
	f.push(t, echo)
	if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
		t.Fatalf("echo ingress = %+v, %v; want it recorded as a new message", r.receipt, r.err)
	}
	p.store.at(beforeCommit, (*unitLog).settles, p.store.crash)
	sending.open()
	p.crashed(t)
	stop()

	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	got := p.op(t, op)
	wantOpState(t, got, model.ProviderAccepted, "")
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
	msgs := f.messages(t)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the outbound message and its inbound copy", len(msgs))
	}
	var out, copied model.Message
	for _, m := range msgs {
		if m.Origin != nil {
			out = m
		} else {
			copied = m
		}
	}
	if got.ResultMessage == nil || out.Ref != *got.ResultMessage {
		t.Fatalf("outbound message = %+v, want the reply's", out.Ref)
	}
	if copied.EchoOf == nil || *copied.EchoOf != out.Ref || copied.Redacted || len(copied.Content.Parts) == 0 {
		t.Fatalf("inbound copy = %+v, want it kept and linked to %v", copied, out.Ref)
	}
	pref := f.p.ref(sent)
	for _, x := range out.External {
		if x == pref {
			t.Fatalf("outbound references %v hold the copy's provider message ID", out.External)
		}
	}
	var updates int
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind != model.EventMessageUpdated || ev.Resource != copied.Ref.Ref() {
			continue
		}
		var pl model.MessageUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
			t.Fatal(err)
		}
		if pl.Change != model.MessageChangeEcho || pl.Operation == nil || *pl.Operation != op {
			t.Fatalf("message.updated payload = %+v, want an echo change naming the operation", pl)
		}
		updates++
	}
	if updates != 1 {
		t.Fatalf("message.updated events for the copy = %d, want 1", updates)
	}
}

// lastSend returns the latest send that reached the provider.
func (p *faultProvider) lastSend() adapter.SendRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sends[len(p.sends)-1]
}

// The provider applies an edit of an inbound message and loses the answer,
// and the process dies. The provider's marked echo of the edit then
// arrives. Ingress records it as evidence only: the message is unchanged
// until the next owner settles the edit from it, applying the content
// once under the caller, with one edited event and no second provider
// edit.
func echoOfEditSettlesAfterRestart(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	f.p.mutations = true
	f.p.respond(applyLost)
	p := f.start(t)
	_, stop := p.receive()
	f.push(t, f.message("evt-1", "m-1"))
	if r := f.accept(t); r.err != nil || !r.receipt.Committed {
		t.Fatalf("ingress = %+v, %v; want committed", r.receipt, r.err)
	}
	stop()
	held := f.messages(t)[0]
	op := p.edit(t, "op-edit", held, "corrected")
	p.work()
	p.idle(t)
	wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)

	p = f.restart(t, cross)
	_, stop = p.receive()
	echo := f.message("evt-echo", "m-1")
	echo.Envelope.Kind, echo.Envelope.SourceRevision, echo.Envelope.OccurredAt = model.EnvelopeMessageEdited, "2", f.fx.Now().Add(time.Second)
	echo.Envelope.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "corrected"}}}
	echo.Envelope.References = []model.ExternalRef{model.EchoRef(f.ch, op, f.p.edited()[0].Attempt)}
	f.push(t, echo)
	if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
		t.Fatalf("echo ingress = %+v, %v; want it committed as evidence", r.receipt, r.err)
	}
	stop()
	if got := f.messages(t)[0]; got.Revision != held.Revision || got.Content.Parts[0].Text != held.Content.Parts[0].Text {
		t.Fatalf("message after the echo = %+v, want it untouched until the edit settles", got)
	}

	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	got := f.messages(t)[0]
	if got.Content.Parts[0].Text != "corrected" || got.LastEdit == nil || got.LastEdit.Operation == nil || *got.LastEdit.Operation != op {
		t.Fatalf("message = %+v, want the edit applied under its operation", got)
	}
	var edits int
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind != model.EventMessageUpdated {
			continue
		}
		var pl model.MessageUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
			t.Fatal(err)
		}
		if pl.Change == model.MessageChangeEdited {
			edits++
		}
	}
	if edits != 1 || len(f.p.edited()) != 1 {
		t.Fatalf("edited events = %d, provider edits = %d; want 1 and 1", edits, len(f.p.edited()))
	}
}
