package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Settled reports whether an operation state is final: no further attempt
// or reconciliation runs for it. Every other state, dead_lettered and
// held included, is unsettled.
func Settled(s model.DeliveryState) bool {
	switch s {
	case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected, model.Committed, model.Expired:
		return true
	}
	return false
}

// withPending returns rec's operation as the caller may see it, with
// model.Operation.Pending derived from the stored intent of an unsettled
// mutation. Pending is never stored: it is revealed only while the target
// message is present, unredacted and readable by the caller under its
// current audience (model.ActionMessageRead, asked with Client.Decide).
// A denial, a missing or redacted target, or an intent that no longer
// yields a valid change omits it without failing; an authorizer or store
// failure is returned.
func withPending(ctx context.Context, c *engine.Client, rec store.OperationRecord) (model.Operation, error) {
	op := Shown(rec)
	if Settled(op.State) {
		return op, nil
	}
	if ps, ok := pendingStateOf(op.Ref, rec.Command); ok {
		return withPendingState(ctx, c, op, ps)
	}
	target, p, ok := pendingOf(op.Ref, rec.Command)
	if !ok {
		return op, nil
	}
	var m model.Message
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) (err error) {
		m, err = r.GetMessage(ctx, target)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return op, nil
	case err != nil:
		return model.Operation{}, err
	case m.Ref != target || m.Redacted:
		return op, nil
	}
	d, err := c.Decide(ctx, engine.MessageRead(m), m.Ref.Ref())
	switch {
	case model.IsCode(err, model.NotFound):
		return op, nil
	case err != nil:
		return model.Operation{}, err
	case !d.Allow:
		return op, nil
	}
	op.Pending = &p
	return op, nil
}

// withPendingState reveals ps on op while the conversation is present and
// readable by the caller (model.ActionConversationRead, asked with
// Client.Decide); a denial or a missing conversation omits it without
// failing, an authorizer or store failure is returned.
func withPendingState(ctx context.Context, c *engine.Client, op model.Operation, ps model.PendingState) (model.Operation, error) {
	var conv model.Conversation
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) (err error) {
		conv, err = r.GetConversation(ctx, ps.Conversation)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return op, nil
	case err != nil:
		return model.Operation{}, err
	case conv.Ref != ps.Conversation:
		return op, nil
	}
	d, err := c.Decide(ctx, engine.ConversationRead(conv), conv.Ref.Ref())
	switch {
	case model.IsCode(err, model.NotFound):
		return op, nil
	case err != nil:
		return model.Operation{}, err
	case !d.Allow:
		return op, nil
	}
	op.PendingState = &ps
	return op, nil
}

// pendingOf decodes a mutation intent into its target and pending change,
// reporting false for any other intent or one that is unreadable or
// yields no valid change (such as a scrubbed one).
func pendingOf(op model.OperationRef, intent model.Extension) (model.MessageRef, model.PendingMutation, bool) {
	var base MutationIntent
	p := model.PendingMutation{Operation: op}
	switch intent.Name {
	case IntentEdit:
		in, err := DecodeEditIntent(intent)
		if err != nil {
			return model.MessageRef{}, p, false
		}
		content := in.Content.Clone()
		base, p.Content = in.MutationIntent, &content
	case IntentDelete:
		in, err := DecodeDeleteIntent(intent)
		if err != nil {
			return model.MessageRef{}, p, false
		}
		base, p.Delete = in.MutationIntent, true
	case IntentReact:
		in, err := DecodeReactIntent(intent)
		if err != nil {
			return model.MessageRef{}, p, false
		}
		base, p.Reaction = in.MutationIntent, &model.PendingReaction{Emoji: in.Emoji, Remove: in.Remove}
	default:
		return model.MessageRef{}, p, false
	}
	p.BaseRevision = base.Base.Revision
	if base.Base.LastEdit != nil {
		e := base.Base.LastEdit.Clone()
		p.BaseLastEdit = &e
	}
	if p.Validate() != nil {
		return model.MessageRef{}, p, false
	}
	return base.Message, p, true
}

// Present returns rec's operation as the caller may see it, with Pending
// derived (see withPending). The caller has authorized the read
// (ReadAuthorization).
func Present(ctx context.Context, c *engine.Client, rec store.OperationRecord) (model.Operation, error) {
	return withPending(ctx, c, rec)
}
