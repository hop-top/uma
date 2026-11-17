package enginetest

import (
	"context"
	"fmt"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// MutationCall is one edit, delete or react request that reached the
// provider.
type MutationCall struct {
	Capability model.Capability
	Effect     adapter.Effect
	Content    model.Content
	Emoji      string
	Remove     bool
	// Receipt is a receipt's kind.
	Receipt string
	// Participant is a membership change's participant, Remove whether it
	// is removed.
	Participant model.ExternalRef
	// State is a provider state request's fields; nil for other calls.
	State *StateCall
}

// WithRefetch declares the refetch capability.
func WithRefetch(_ *engine.Config, f *OutFactory) { f.Refetches = true }

// WithSingleReactions declares single-reaction cardinality.
func WithSingleReactions(_ *engine.Config, f *OutFactory) { f.SingleReaction = true }

// MutationCalls returns the mutation requests that reached the provider.
func (f *OutFactory) MutationCalls() []MutationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.mutations)
}

// Edit answers like a send, consuming the same script.
func (i *OutInstance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	return i.mutate(ctx, MutationCall{Capability: model.CapabilityEdit, Effect: r.Effect, Content: r.Content.Clone()})
}

// Delete answers like a send, consuming the same script.
func (i *OutInstance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return i.mutate(ctx, MutationCall{Capability: model.CapabilityDelete, Effect: r.Effect})
}

// React answers like a send, consuming the same script. An applied
// reaction reports the provider message that carried it.
func (i *OutInstance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return i.mutate(ctx, MutationCall{Capability: model.CapabilityReact, Effect: r.Effect, Emoji: r.Emoji, Remove: r.Remove})
}

// Refetch has no provider copy to read.
func (i *OutInstance) Refetch(context.Context, adapter.RefetchRequest) (model.Envelope, error) {
	i.F.mu.Lock()
	i.F.log = append(i.F.log, "refetch")
	i.F.mu.Unlock()
	return model.Envelope{}, model.NewError(model.NotFound, "no provider copy")
}

func (i *OutInstance) mutate(ctx context.Context, c MutationCall) (adapter.Outcome, error) {
	i.F.mu.Lock()
	var fn SendFunc
	if len(i.F.script) > 0 {
		fn, i.F.script = i.F.script[0], i.F.script[1:]
	}
	i.F.mutations = append(i.F.mutations, c)
	i.F.log = append(i.F.log, string(c.Capability))
	i.F.mu.Unlock()
	if fn == nil {
		fn = Accepted
	}
	req := adapter.SendRequest{Effect: c.Effect, Content: c.Content}
	out, err, applied := fn(ctx, req)
	if !applied {
		return out, err
	}
	i.F.mu.Lock()
	i.F.applied = append(i.F.applied, req)
	if c.Capability == model.CapabilityReact && len(out.References) == 0 && err == nil && out.State != model.Unknown {
		i.F.seq++
		out.References = []model.ExternalRef{model.ProviderMessageRef(c.Effect.Route.Channel, fmt.Sprintf("reaction-%d", i.F.seq))}
	}
	i.F.mu.Unlock()
	return out, err
}

// Target is the provider message ID of the rig's seeded target message.
const Target = "pm-target"

// SeedMessage stores an inbound public message holding provider message
// Target on the rig's channel in the rig's conversation.
func (r *Rig) SeedMessage(text string) model.Message {
	r.T.Helper()
	m := model.Message{
		Ref:      model.MessageRef{Conversation: r.Conv.Ref, ID: "00000005-0000-7000-8000-000000000001"},
		Audience: model.Audience{Visibility: model.Public},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
		External: []model.ExternalRef{model.ProviderMessageRef(r.Ch.Ref, Target)},
		Source:   OutKey, OccurredAt: r.Clock.Now().UTC(), RecordedAt: r.Clock.Now().UTC(), Revision: "m1",
	}
	if err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error { return tx.PutMessage(r.Ctx, m, "") }); err != nil {
		r.T.Fatalf("seed message: %v", err)
	}
	return m
}

// Message reads a stored message.
func (r *Rig) Message(ref model.MessageRef) model.Message {
	r.T.Helper()
	var m model.Message
	err := r.E.Store().View(r.Ctx, TenantA, func(rd store.Reader) (err error) {
		m, err = rd.GetMessage(r.Ctx, ref)
		return err
	})
	if err != nil {
		r.T.Fatalf("GetMessage: %v", err)
	}
	return m
}

// Mutate rewrites a stored message under a new revision, as an inbound
// change would.
func (r *Rig) Mutate(ref model.MessageRef, fn func(*model.Message)) model.Message {
	r.T.Helper()
	var out model.Message
	err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error {
		m, err := tx.GetMessage(r.Ctx, ref)
		if err != nil {
			return err
		}
		prev := m.Revision
		fn(&m)
		if m.Revision, err = operations.NewRevision(r.Ctx, r.E); err != nil {
			return err
		}
		out = m
		return tx.PutMessage(r.Ctx, m, prev)
	})
	if err != nil {
		r.T.Fatalf("mutate message: %v", err)
	}
	return out
}

// Edit submits an edit of m to text at m's revision.
func (r *Rig) Edit(opID model.OperationID, m model.Message, text string) model.Acceptance {
	r.T.Helper()
	a, err := r.C.Messages().Edit(r.Ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "corr-edit"}, Operation: opID, ExpectedRevision: m.Revision},
		Message: m.Ref, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		r.T.Fatalf("Edit(%s): %v", opID, err)
	}
	return a
}

// Delete submits a delete of m at m's revision.
func (r *Rig) Delete(opID model.OperationID, m model.Message) model.Acceptance {
	r.T.Helper()
	a, err := r.C.Messages().Delete(r.Ctx, model.DeleteMessage{
		Command: model.Command{Call: model.Call{Correlation: "corr-delete"}, Operation: opID, ExpectedRevision: m.Revision},
		Message: m.Ref,
	})
	if err != nil {
		r.T.Fatalf("Delete(%s): %v", opID, err)
	}
	return a
}

// React submits adding, or removing, emoji on m.
func (r *Rig) React(opID model.OperationID, m model.Message, emoji string, remove bool) model.Acceptance {
	r.T.Helper()
	a, err := r.C.Messages().React(r.Ctx, model.Reaction{
		Command: model.Command{Call: model.Call{Correlation: "corr-react"}, Operation: opID},
		Message: m.Ref, Emoji: emoji, Remove: remove,
	})
	if err != nil {
		r.T.Fatalf("React(%s): %v", opID, err)
	}
	return a
}
