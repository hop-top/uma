package widget

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// EphemeralTTL is how long a visitor is shown an ephemeral interaction
// response. The respond window of the interaction bounds when the response
// may be given, not how long it stays on screen.
const EphemeralTTL = 15 * time.Minute

// Respond answers an interaction the visitor sent. The widget has no
// response token: it answers through the visitor's log, like any other
// delivery, and the visitor's event stream carries the interaction.response
// event, so a response is applied once the log stored it and a lost answer
// settles outcome_unknown, as for a message.
//
// Mode message adds a new message, ephemeral shows the content to this
// visitor only for EphemeralTTL, and update replaces the content of the
// message the control sits on (Effect.Target), as an edit does. Message and
// update are public; an ephemeral response is restricted. A follow-up is
// delivered the same way. The widget never falls back to another mode or a
// wider audience.
func (x *Instance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	if err := x.authorize(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	ic := r.Interaction
	if ic == nil || ic.ProviderID == "" || ic.Ref.IsZero() {
		return adapter.Outcome{}, model.NewError(model.Validation, "response names no interaction")
	}
	key, session, err := x.target(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := checkContent(r.Content); err != nil {
		return adapter.Outcome{}, err
	}
	ev := Event{Type: EventInteractionResponse, Interaction: ic.ProviderID, Mode: r.Mode, FollowUp: r.FollowUp, Parts: parts(r.Content)}
	e := Entry{Event: ev, Session: session, Operation: r.Operation, Interaction: ic.Ref}
	switch r.Mode {
	case model.ResponseModeMessage:
		if err := public(r.Audience); err != nil {
			return adapter.Outcome{}, err
		}
		if e.Event.Message, err = randomID("m"); err != nil {
			return adapter.Outcome{}, err
		}
		return x.deliver(ctx, key, e, MessageRef(x.ref(), e.Event.Message))
	case model.ResponseModeEphemeral:
		if r.Audience.Visibility != model.Restricted {
			return adapter.Outcome{}, model.NewError(model.Validation, "an ephemeral response is restricted to its visitor")
		}
		if e.Event.Message, err = randomID("m"); err != nil {
			return adapter.Outcome{}, err
		}
		e.ExpiresAt = x.now().Add(EphemeralTTL)
		return x.deliver(ctx, key, e, MessageRef(x.ref(), e.Event.Message))
	case model.ResponseModeUpdate:
		if err := public(r.Audience); err != nil {
			return adapter.Outcome{}, err
		}
		if e.Event.Message, err = x.mutationTarget(r.Target); err != nil {
			return adapter.Outcome{}, err
		}
		return x.deliver(ctx, key, e)
	}
	return adapter.Outcome{}, model.NewError(model.Validation, "unknown response mode")
}

var _ adapter.InteractionResponder = (*Instance)(nil)
