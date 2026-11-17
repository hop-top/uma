package conformance

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// serviceMutationsExecuted runs edits, reactions and deletes through
// Runtime.Run on the store under test: each changes its message only once
// the provider applied it, targets the message's provider message, and
// emits message.updated naming the operation without content. A
// reaction's provider reference stays on its entry, never on the message.
// An edit answered after a newer inbound edit leaves that edit in place,
// emits edit_superseded and queues a refetch under the editor; a delete
// tombstones the message, reactions included.
func serviceMutationsExecuted(t *testing.T, factory StoreFactory) {
	env := newMutationEnv(t, factory)
	msgs := env.client.Messages()
	target := env.target
	content := func(s string) model.Content {
		return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
	}

	edit, err := msgs.Edit(env.ctx, model.EditMessage{Command: env.cmd(target.Revision), Message: target.Ref, Content: content("corrected")})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if op := env.deliver(edit.Operation.Ref); op.State != model.ProviderAccepted || op.Pending != nil {
		t.Fatalf("edit settled %+v", op)
	}
	m := env.message(target.Ref)
	if m.Content.Parts[0].Text != "corrected" || m.LastEdit == nil || m.LastEdit.Actor != mutationActor ||
		m.LastEdit.Operation == nil || *m.LastEdit.Operation != edit.Operation.Ref {
		t.Fatalf("message after the edit = %+v", m)
	}
	env.requireUpdate(edit.Operation.Ref, model.MessageChangeEdited)

	react, err := msgs.React(env.ctx, model.Reaction{Command: env.cmd(""), Message: target.Ref, Emoji: "👍"})
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	rop := env.deliver(react.Operation.Ref)
	m = env.message(target.Ref)
	if len(m.Reactions) != 1 || m.Reactions[0].Actor != mutationActor || m.Reactions[0].By != nil ||
		!slices.Equal(m.Reactions[0].External, rop.ProviderReferences) || len(rop.ProviderReferences) != 1 ||
		!slices.Equal(m.External, target.External) {
		t.Fatalf("message after the reaction = %+v (operation %+v)", m, rop)
	}
	env.requireUpdate(react.Operation.Ref, model.MessageChangeReactionAdded)

	race, err := msgs.Edit(env.ctx, model.EditMessage{Command: env.cmd(m.Revision), Message: target.Ref, Content: content("older")})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	env.provider.hold(func(model.Capability) {
		env.put(func(tx store.Tx) error {
			cur, err := tx.GetMessage(env.ctx, target.Ref)
			if err != nil {
				return err
			}
			prev := cur.Revision
			cur.Content, cur.Revision = content("newer"), prev+"-inbound"
			cur.LastEdit = &model.Edit{Actor: "sink", At: env.now()}
			return tx.PutMessage(env.ctx, cur, prev)
		})
	})
	if op := env.deliver(race.Operation.Ref); op.State != model.ProviderAccepted || op.Failure != nil {
		t.Fatalf("raced edit settled %+v", op)
	}
	env.provider.hold(nil)
	if m = env.message(target.Ref); m.Content.Parts[0].Text != "newer" {
		t.Fatalf("message after the raced edit = %+v, want the newer inbound edit", m)
	}
	env.requireUpdate(race.Operation.Ref, model.MessageChangeEditSuperseded)
	if err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		_, err := r.OperationByKey(env.ctx, store.OperationKey{
			Tenant: serviceTenant, Caller: mutationActor, Kind: "uma.provider.refetch", Target: target.Ref.Ref(),
			ID: model.OperationID("uma.refetch." + string(race.Operation.Ref.ID)),
		})
		return err
	}); err != nil {
		t.Fatalf("refetch of the superseded edit: %v", err)
	}

	del, err := msgs.Delete(env.ctx, model.DeleteMessage{Command: env.cmd(m.Revision), Message: target.Ref})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	env.deliver(del.Operation.Ref)
	m = env.message(target.Ref)
	if !m.Deleted || len(m.Content.Parts) != 0 || m.Reactions != nil {
		t.Fatalf("message after the delete = %+v, want a tombstone", m)
	}
	env.requireUpdate(del.Operation.Ref, model.MessageChangeDeleted)
	want := []model.Capability{model.CapabilityEdit, model.CapabilityReact, model.CapabilityEdit, model.CapabilityDelete}
	if got := env.provider.log(); !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
	for _, e := range env.provider.calls {
		if e.Target == nil || *e.Target != env.provRef {
			t.Fatalf("effect %+v, want the stored provider message %v", e, env.provRef)
		}
	}
}

func (env *mutationEnv) message(ref model.MessageRef) model.Message {
	env.t.Helper()
	var m model.Message
	if err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) {
		m, err = r.GetMessage(env.ctx, ref)
		return err
	}); err != nil {
		env.t.Fatalf("GetMessage: %v", err)
	}
	return m
}

// requireUpdate requires exactly one message.updated event naming op,
// with change and without content.
func (env *mutationEnv) requireUpdate(op model.OperationRef, change string) {
	env.t.Helper()
	var found []string
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		req := model.Filter{Kinds: []string{model.EventMessageUpdated}}
		var after model.Cursor
		for {
			p, err := r.Events(env.ctx, req, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				var keys map[string]json.RawMessage
				var pl model.MessageUpdatedPayload
				if json.Unmarshal(ev.Payload.Data, &keys) != nil || json.Unmarshal(ev.Payload.Data, &pl) != nil {
					env.t.Fatalf("payload %s", ev.Payload.Data)
				}
				if pl.Operation == nil || *pl.Operation != op {
					continue
				}
				if _, ok := keys["content"]; ok {
					env.t.Fatalf("message.updated carries content: %s", ev.Payload.Data)
				}
				found = append(found, pl.Change)
			}
			if p.Complete {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		env.t.Fatal(err)
	}
	if !slices.Equal(found, []string{change}) {
		env.t.Fatalf("message.updated changes of %v = %v, want [%s]", op, found, change)
	}
}
