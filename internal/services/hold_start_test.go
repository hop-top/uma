package services_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// startWorld is a start rig whose proposer is a plugin actor and whose
// decider is the rig's own principal. atHold selects the conversation
// creation mode of Config.Held.
type startWorld struct {
	*rig
	in     model.InboxRef
	plugin enginetest.Client
	atHold bool
}

func newStartWorld(t *testing.T, atHold bool) *startWorld {
	t.Helper()
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, func(c *engine.Config, _ *outFactory) {
		c.Held.StartCreateAtHold = atHold
	})
	plugin, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	return &startWorld{rig: r, in: in, plugin: plugin, atHold: atHold}
}

func (w *startWorld) hold(op model.OperationID, to string, text string) (model.Acceptance, error) {
	req := startReq(op, w.in, recipient(w.rig, to))
	req.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
	exp := w.Clock.Now().Add(time.Hour).UTC()
	req.Hold = &model.HoldProposal{Reason: "outreach", TrustKey: "triage/start", ExpiresAt: &exp}
	return w.plugin.Conversations().Start(w.Ctx, req)
}

func (w *startWorld) conversations() []model.Conversation {
	w.T.Helper()
	var out []model.Conversation
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) error {
		p, err := r.Conversations(w.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{w.in}}, Page: model.PageRequest{Limit: 50}})
		out = p.Items
		return err
	}); err != nil {
		w.T.Fatal(err)
	}
	return out
}

func (w *startWorld) messages(conv model.ConversationRef) []model.Message {
	w.T.Helper()
	var out []model.Message
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) error {
		p, err := r.Messages(w.Ctx, model.ListRequest{Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 50}})
		out = p.Items
		return err
	}); err != nil {
		w.T.Fatal(err)
	}
	return out
}

func (w *startWorld) decide(op model.Operation) model.DecideHeld {
	return model.DecideHeld{Call: call(), Operation: op.Ref, ExpectedRevision: op.Revision}
}

func eachStartMode(t *testing.T, fn func(t *testing.T, w *startWorld)) {
	for _, atHold := range []bool{false, true} {
		name := "create on approve"
		if atHold {
			name = "create at hold"
		}
		t.Run(name, func(t *testing.T) { fn(t, newStartWorld(t, atHold)) })
	}
}

// A start is holdable in both modes. Held, nothing is delivered; the
// conversation exists only when the config creates it at hold, and the
// result references are named at hold only then.
func TestHeldStartHold(t *testing.T) {
	eachStartMode(t, func(t *testing.T, w *startWorld) {
		a, err := w.hold("op-1", "u-1", "hello")
		if err != nil || a.Operation.State != model.Held || a.Operation.Held == nil || a.Operation.Held.Intent == nil {
			t.Fatalf("hold = %+v, %v", a.Operation, err)
		}
		op := a.Operation
		if !op.Held.Applies || op.Held.Local {
			t.Fatalf("held block = %+v, want a start intent applied locally, not local-only", op.Held)
		}
		convs := w.conversations()
		if w.atHold {
			if len(convs) != 1 || op.Result == nil || op.ResultMessage == nil || op.ResultMessage.Conversation != convs[0].Ref {
				t.Fatalf("at hold: conversations %d, result %+v %+v", len(convs), op.Result, op.ResultMessage)
			}
			if got := w.messages(convs[0].Ref); len(got) != 0 {
				t.Fatalf("message written before approval: %v", got)
			}
		} else if len(convs) != 0 || op.Result != nil || op.ResultMessage != nil {
			t.Fatalf("on approve: conversations %d, result %+v %+v; want nothing", len(convs), op.Result, op.ResultMessage)
		}
		if n := w.Step("w"); n != 0 || len(w.F.ProviderSends()) != 0 {
			t.Fatalf("a held start queued %d work items, sent %d", n, len(w.F.ProviderSends()))
		}
		again, err := w.hold("op-1", "u-1", "hello")
		if err != nil || !again.Replayed || again.Operation.Ref != op.Ref || len(w.conversations()) != len(convs) {
			t.Fatalf("replayed hold = %+v, %v", again, err)
		}
	})
}

// Approval creates the conversation (on approve), writes the message once
// under the reserved ID and delivers it; a second approval is a replay.
func TestHeldStartApprove(t *testing.T) {
	eachStartMode(t, func(t *testing.T, w *startWorld) {
		a, err := w.hold("op-1", "u-1", "hello")
		if err != nil {
			t.Fatal(err)
		}
		held := a.Operation
		d, err := w.C.Operations().Approve(w.Ctx, w.decide(held))
		if err != nil || d.Operation.State != model.Queued || d.Replayed {
			t.Fatalf("Approve = %+v, %v", d.Operation, err)
		}
		convs := w.conversations()
		if len(convs) != 1 {
			t.Fatalf("conversations = %d, want one", len(convs))
		}
		op := d.Operation
		if op.ResultMessage == nil || op.Result == nil || op.ResultMessage.Conversation != convs[0].Ref || op.Result.Kind != model.KindMessage {
			t.Fatalf("result after approval = %+v %+v", op.Result, op.ResultMessage)
		}
		if w.atHold && (held.ResultMessage == nil || *held.ResultMessage != *op.ResultMessage) {
			t.Fatalf("approval changed the references named at hold: %+v -> %+v", held.ResultMessage, op.ResultMessage)
		}
		msgs := w.messages(convs[0].Ref)
		if len(msgs) != 1 || msgs[0].Ref != *op.ResultMessage || msgs[0].Origin == nil || msgs[0].Origin.Actor != "plugin:triage" ||
			msgs[0].Origin.Operation != held.Ref {
			t.Fatalf("messages = %+v, want the start's message under the proposer", msgs)
		}
		w.Step("w")
		if sent := w.F.ProviderSends(); len(sent) != 1 || sent[0].Content.Parts[0].Text != "hello" {
			t.Fatalf("provider sends = %+v, want one", sent)
		}
		if st := w.Op(held.Ref).State; st != model.ProviderAccepted {
			t.Fatalf("state = %s, want provider_accepted", st)
		}
		again, err := w.C.Operations().Approve(w.Ctx, w.decide(d.Operation))
		if err != nil || !again.Replayed || len(w.conversations()) != 1 || len(w.messages(convs[0].Ref)) != 1 {
			t.Fatalf("second Approve = %+v, %v", again.Operation, err)
		}
		w.Step("w")
		if n := len(w.F.ProviderSends()); n != 1 {
			t.Fatalf("a replayed approval sent again (%d sends)", n)
		}
	})
}

// A rejected or expired start sends nothing and writes no message. Created
// on approve, it leaves no conversation either; created at hold, the
// conversation it opened stays.
func TestHeldStartRejectAndExpireLeaveNothing(t *testing.T) {
	eachStartMode(t, func(t *testing.T, w *startWorld) {
		want := 0
		if w.atHold {
			want = 1
		}
		rej, err := w.hold("op-r", "u-1", "hello")
		if err != nil {
			t.Fatal(err)
		}
		if d, err := w.C.Operations().Reject(w.Ctx, w.decide(rej.Operation)); err != nil || d.Operation.State != model.Rejected {
			t.Fatalf("Reject = %+v, %v", d.Operation, err)
		}
		exp, err := w.hold("op-e", "u-2", "hello")
		if err != nil {
			t.Fatal(err)
		}
		w.Clock.Advance(2 * time.Hour)
		if rep, err := operations.ExpireHeld(w.Ctx, w.E, tenantA, 10*time.Minute); err != nil || rep.Expired != 1 {
			t.Fatalf("ExpireHeld = %+v, %v", rep, err)
		}
		if got := w.Op(exp.Operation.Ref); got.State != model.Expired {
			t.Fatalf("expired = %s", got.State)
		}
		convs := w.conversations()
		if len(convs) != 2*want {
			t.Fatalf("conversations = %d, want %d", len(convs), 2*want)
		}
		for _, c := range convs {
			if got := w.messages(c.Ref); len(got) != 0 {
				t.Fatalf("a message was written by an undecided start: %v", got)
			}
		}
		if n := w.Step("w"); n != 0 || len(w.F.ProviderSends()) != 0 {
			t.Fatalf("queued %d, sent %d", n, len(w.F.ProviderSends()))
		}
	})
}

// An edit changes the content and nothing else; the recipient, the inbox
// and the conversation are the claim's.
func TestHeldStartEdit(t *testing.T) {
	eachStartMode(t, func(t *testing.T, w *startWorld) {
		a, err := w.hold("op-1", "u-1", "hello")
		if err != nil {
			t.Fatal(err)
		}
		var rec store.OperationRecord
		if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
			rec, err = r.Operation(w.Ctx, a.Operation.Ref)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		_ = json.Unmarshal(rec.Command.Data, &raw)
		edit := func(f func(m map[string]any)) model.EditHeld {
			var m map[string]any
			_ = json.Unmarshal(rec.Command.Data, &m)
			f(m)
			data, _ := json.Marshal(m)
			return model.EditHeld{DecideHeld: w.decide(a.Operation), Args: model.Extension{Name: operations.IntentStart, Version: 1, Data: data}}
		}
		other, _ := json.Marshal(model.InboxRef{Tenant: tenantA, ID: "00000004-0000-7000-8000-0000000000ff"})
		var otherInbox any
		_ = json.Unmarshal(other, &otherInbox)
		if _, err := w.C.Operations().Edit(w.Ctx, edit(func(m map[string]any) { m["inbox"] = otherInbox })); !model.IsCode(err, model.Validation) {
			t.Fatalf("retarget edit = %v, want validation", err)
		}
		if raw["recipient"] != nil {
			if _, err := w.C.Operations().Edit(w.Ctx, edit(func(m map[string]any) { m["recipient"].(map[string]any)["id"] = "someone-else" })); !model.IsCode(err, model.Validation) {
				t.Fatalf("recipient edit = %v, want validation", err)
			}
		}
		d, err := w.C.Operations().Edit(w.Ctx, edit(func(m map[string]any) {
			m["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] = "hello, edited"
		}))
		if err != nil || d.Decision != model.HeldEdited {
			t.Fatalf("Edit = %+v, %v", d, err)
		}
		w.Step("w")
		if sent := w.F.ProviderSends(); len(sent) != 1 || sent[0].Content.Parts[0].Text != "hello, edited" {
			t.Fatalf("provider sends = %+v, want the edited text", sent)
		}
		convs := w.conversations()
		if len(convs) != 1 || len(w.messages(convs[0].Ref)) != 1 {
			t.Fatalf("conversations %d", len(convs))
		}
	})
}

// With the inbox's selection reusing a conversation, an on-approve start
// lands in the conversation the approval selects, not a new one; the
// result names it.
func TestHeldStartOnApproveSelectsExistingConversation(t *testing.T) {
	w := newStartWorld(t, false)
	first, err := w.hold("op-1", "u-1", "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.C.Operations().Approve(w.Ctx, w.decide(first.Operation)); err != nil {
		t.Fatal(err)
	}
	second, err := w.hold("op-2", "u-1", "two")
	if err != nil {
		t.Fatal(err)
	}
	if second.AlreadyHeld {
		t.Fatalf("second start answered as already held: %+v", second)
	}
	d, err := w.C.Operations().Approve(w.Ctx, w.decide(second.Operation))
	if err != nil {
		t.Fatal(err)
	}
	convs := w.conversations()
	if len(convs) != 1 || d.Operation.ResultMessage.Conversation != convs[0].Ref {
		t.Fatalf("conversations %d, result %+v; want the existing conversation reused", len(convs), d.Operation.ResultMessage)
	}
	if got := w.messages(convs[0].Ref); len(got) != 2 {
		t.Fatalf("messages = %d, want both starts in it", len(got))
	}
	w.Step("w")
	w.Step("w")
	if n := len(w.F.ProviderSends()); n != 2 {
		t.Fatalf("provider sends = %d, want 2", n)
	}
}

// A start that is not held is unchanged by the config: it still creates its
// conversation in its claiming unit and queues its message.
func TestStartWithoutHoldIsUnchanged(t *testing.T) {
	eachStartMode(t, func(t *testing.T, w *startWorld) {
		a, err := w.C.Conversations().Start(w.Ctx, startReq("op-1", w.in, recipient(w.rig, "u-1")))
		if err != nil || a.Operation.State != model.Queued || a.Operation.ResultMessage == nil || len(w.conversations()) != 1 {
			t.Fatalf("Start = %+v, %v", a.Operation, err)
		}
	})
}
