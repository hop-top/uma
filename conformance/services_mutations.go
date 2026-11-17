package conformance

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// serviceMutations runs Messages.Edit, Delete and React through the
// operation ledger: a new claim is queued with the message as its result
// and the message untouched; a repeat replays and another payload
// conflicts; a stale revision, a live stream's message and a send-only
// channel are refused before anything is queued; Operations.Get shows the
// pending change only to a caller who may read the message, and none once
// the operation settles.
func serviceMutations(t *testing.T, factory StoreFactory) {
	env := newMutationEnv(t, factory)
	msgs := env.client.Messages()
	target := env.target
	content := model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "corrected"}}}
	edit := model.EditMessage{Command: env.cmd(target.Revision), Message: target.Ref, Content: content}
	a, err := msgs.Edit(env.ctx, edit)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	op := a.Operation
	if op.State != model.Queued || a.Replayed || op.ResultMessage == nil || *op.ResultMessage != target.Ref || *op.Result != target.Ref.Ref() || op.Pending != nil {
		t.Fatalf("Edit acceptance = %+v", a)
	}
	got, err := msgs.Get(env.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "g"}, Ref: target.Ref})
	if err != nil || got.Revision != target.Revision || got.Content.Parts[0].Text != "original" {
		t.Fatalf("message after claim = %+v, %v; want it untouched", got, err)
	}

	again, err := msgs.Edit(env.ctx, edit)
	if err != nil || !again.Replayed || again.Operation.Ref != op.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	other := edit
	other.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "other"}}}
	_, err = msgs.Edit(env.ctx, other)
	serviceCode(t, err, model.Conflict)

	_, err = msgs.Delete(env.ctx, model.DeleteMessage{Command: env.cmd("stale"), Message: target.Ref})
	serviceCode(t, err, model.Conflict)
	_, err = msgs.React(env.ctx, model.Reaction{Command: env.cmd(""), Message: env.plain.Ref, Emoji: "👍"})
	serviceCode(t, err, model.Unsupported)
	live := model.Stream{
		Ref: model.StreamRef{Conversation: target.Ref.Conversation, ID: env.id()}, Mode: model.StreamEdit, State: model.StreamOpen,
		Audience: model.Audience{Visibility: model.Public}, ProviderReferences: []model.ExternalRef{env.provRef}, Revision: "s1",
	}
	env.put(func(tx store.Tx) error { return tx.PutStream(env.ctx, live, "") })
	_, err = msgs.React(env.ctx, model.Reaction{Command: env.cmd(""), Message: target.Ref, Emoji: "👍"})
	serviceCode(t, err, model.Conflict)
	done := live.Clone()
	done.State, done.Revision = model.StreamFinished, "s2"
	env.put(func(tx store.Tx) error { return tx.PutStream(env.ctx, done, "s1") })
	r, err := msgs.React(env.ctx, model.Reaction{Command: env.cmd(""), Message: target.Ref, Emoji: "👍"})
	if err != nil || r.Operation.State != model.Queued {
		t.Fatalf("React after the stream finished = %+v, %v", r, err)
	}

	p := env.get(op.Ref).Pending
	if p == nil || p.Operation != op.Ref || p.BaseRevision != target.Revision || p.Content == nil || p.Content.Parts[0].Text != "corrected" {
		t.Fatalf("pending = %+v, want the queued edit", p)
	}
	if rp := env.get(r.Operation.Ref).Pending; rp == nil || rp.Reaction == nil || rp.Reaction.Emoji != "👍" {
		t.Fatalf("react pending = %+v", rp)
	}
	env.policy.hide(true)
	if hidden := env.get(op.Ref); hidden.Pending != nil || hidden.State != model.Queued {
		t.Fatalf("pending shown without message.read: %+v", hidden)
	}
	env.policy.hide(false)
	env.settle(op.Ref, model.Delivered)
	if settled := env.get(op.Ref); settled.Pending != nil {
		t.Fatalf("settled edit still pending: %+v", settled.Pending)
	}
	var stored model.Message
	if err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) {
		stored, err = r.GetMessage(env.ctx, target.Ref)
		return err
	}); err != nil || stored.Revision != target.Revision {
		t.Fatalf("ledger wrote the message: %+v, %v", stored, err)
	}
}
