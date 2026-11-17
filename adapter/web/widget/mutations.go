package widget

import (
	"context"
	"encoding/hex"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Edit replaces the content of a message the visitor sees. Uma is the
// widget's provider, so any message of the route's visitor log can be
// edited, one the visitor sent included.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	if err := checkContent(r.Content); err != nil {
		return adapter.Outcome{}, err
	}
	return x.mutate(ctx, r.Effect, Event{Type: EventMessageEdit, Parts: parts(r.Content)})
}

// Delete removes a message from what the visitor sees.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return x.mutate(ctx, r.Effect, Event{Type: EventMessageDelete})
}

// React adds or removes the host's reaction on a message. The widget has
// no reactor identity: reactions belong to the host.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return x.mutate(ctx, r.Effect, Event{Type: EventReaction, Emoji: r.Emoji, Removed: r.Remove})
}

// mutationRefusals are the codes under which a log refuses a mutation for
// good and the outcome keeps the code.
var mutationRefusals = []model.ErrorCode{model.Unauthorized, model.NotFound, model.Unsupported}

// mutation reports whether typ is the event type of a mutation.
func mutation(typ string) bool {
	return typ == EventMessageEdit || typ == EventMessageDelete || typ == EventReaction || typ == EventReceipt
}

// mutate appends ev, naming the effect's target message, to the route's
// visitor log. It reports no reference: a mutation adds none.
func (x *Instance) mutate(ctx context.Context, eff adapter.Effect, ev Event) (adapter.Outcome, error) {
	return x.mutateTarget(ctx, eff, ev, x.mutationTarget)
}

// mutateTarget is mutate with the rule that decides which messages the
// effect's target may name.
func (x *Instance) mutateTarget(ctx context.Context, eff adapter.Effect, ev Event, target func(*model.ExternalRef) (string, error)) (adapter.Outcome, error) {
	if err := x.authorize(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	key, session, err := x.target(eff.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := public(eff.Audience); err != nil {
		return adapter.Outcome{}, err
	}
	if ev.Message, err = target(eff.Target); err != nil {
		return adapter.Outcome{}, err
	}
	return x.deliver(ctx, key, Entry{Event: ev, Session: session, Operation: eff.Operation})
}

// mutationTarget returns the widget message ID t names: a provider message
// reference of this channel with the shape of an ID the widget gave a
// message it sent or one a visitor sent.
func (x *Instance) mutationTarget(t *model.ExternalRef) (string, error) {
	if t == nil || *t != MessageRef(x.ref(), t.ID) || !isAnyMessageID(t.ID) {
		return "", model.NewError(model.Validation, "target is not a message of this channel")
	}
	return t.ID, nil
}

// isAnyMessageID reports whether id has the shape of the ID of a message
// the widget sent ('m') or a visitor sent ('v'): the prefix and 16 random
// or hashed bytes in hex.
func isAnyMessageID(id string) bool {
	if len(id) != 33 || id[0] != 'm' && id[0] != 'v' {
		return false
	}
	_, err := hex.DecodeString(id[1:])
	return err == nil
}

var (
	_ adapter.Editor  = (*Instance)(nil)
	_ adapter.Deleter = (*Instance)(nil)
	_ adapter.Reactor = (*Instance)(nil)
)
