package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"hop.top/uma/model"
)

// ErrResponseClaimed is wrapped by the error Turn.Respond returns when
// another worker already holds the interaction's initial response: uma
// accepts one initial response per interaction and answers the loser
// model.Conflict. A handler that returns it unchanged ends in Ignored.
var ErrResponseClaimed = errors.New("bot: the interaction's initial response is already claimed")

// claimedError is the Conflict of a lost initial claim, matching both
// ErrResponseClaimed and the uma error.
type claimedError struct{ err error }

func (e *claimedError) Error() string   { return e.err.Error() }
func (e *claimedError) Unwrap() []error { return []error{ErrResponseClaimed, e.err} }

// routeInteraction reads the interaction of an interaction.received event
// and selects its route, filling t. It returns a nil handler when no route
// claims the interaction or IsAddressed rejects it.
//
// An interaction that names a command, an action and form fields takes the
// first route that has a handler for it, in that order: providers report
// typed options of a command as form fields, and a control press may carry
// the state of its form.
func (b *Bot) routeInteraction(ctx context.Context, t *Turn) (Handler, error) {
	fallback := b.kinds[model.EventInteractionReceived]
	if len(b.commands) == 0 && len(b.actions) == 0 && len(b.submissions) == 0 && fallback == nil {
		return nil, nil
	}
	ev := t.Event
	if ev.Payload.Name != model.SchemaInteractionReceived || ev.Payload.Version != model.PayloadVersion1 {
		return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "bot: unsupported interaction event payload", Retry: model.NoRetry}
	}
	var p model.InteractionReceivedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil || p.Interaction.Validate() != nil {
		return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "bot: malformed interaction event payload", Retry: model.NoRetry}
	}
	in, err := b.cfg.Client.Interactions().Get(ctx, model.Lookup[model.InteractionRef]{Call: t.call(), Ref: p.Interaction})
	if err != nil {
		return nil, err
	}
	t.Interaction, t.Conversation = &in, in.Ref.Conversation
	if b.cfg.IsAddressed != nil && !b.cfg.IsAddressed(in) {
		return nil, nil
	}
	for _, r := range []struct {
		route  Route
		name   string
		routes map[string]Handler
	}{
		{RouteCommand, in.Command, b.commands},
		{RouteAction, in.Action, b.actions},
		{RouteSubmission, in.Fields.Name, b.submissions},
	} {
		if h := r.routes[r.name]; h != nil {
			t.Route, t.Handler = r.route, string(r.route)+"/"+r.name
			return h, nil
		}
	}
	if fallback != nil {
		t.Route, t.Handler = RouteKind, "kind/"+ev.Kind
	}
	return fallback, nil
}

// Respond answers the turn's interaction with its initial response, in
// mode (a model.ResponseMode* value) to audience: restricted for an
// ephemeral response. The n-th Respond of a turn uses
// OperationID("respond/<n>"), so a rerun handler that responds in the same
// order replays instead of answering twice. The response expires with the
// interaction's respond window, or Config.RespondWindow after it was
// recorded when the provider states none.
//
// uma accepts one initial response per interaction. When another worker
// won it, Respond returns an error wrapping ErrResponseClaimed, which ends
// the turn Ignored; a window that has passed is model.DeadlineExpired,
// which drops the event. A response that lost to a concurrent unit is
// resubmitted as Step resubmits its own units.
func (t *Turn) Respond(ctx context.Context, mode string, audience model.Audience, content model.Content) (model.Acceptance, error) {
	return t.respond(ctx, mode, audience, content, false)
}

// FollowUp sends a follow-up response to the turn's interaction. It claims
// nothing and may precede or follow the initial response. The n-th FollowUp
// of a turn uses OperationID("follow-up/<n>"). Arguments and failures are
// those of Respond, except that a follow-up has no initial claim to lose.
func (t *Turn) FollowUp(ctx context.Context, mode string, audience model.Audience, content model.Content) (model.Acceptance, error) {
	return t.respond(ctx, mode, audience, content, true)
}

func (t *Turn) respond(ctx context.Context, mode string, audience model.Audience, content model.Content, followUp bool) (model.Acceptance, error) {
	in := t.Interaction
	if in == nil {
		return model.Acceptance{}, model.NewError(model.Validation, "bot: the event names no interaction to respond to")
	}
	key, counter := "respond/", &t.responses
	if followUp {
		key, counter = "follow-up/", &t.followUps
	}
	t.mu.Lock()
	n := *counter
	*counter++
	t.mu.Unlock()
	expires := in.RespondUntil
	if expires.IsZero() {
		expires = in.RecordedAt.Add(t.bot.cfg.RespondWindow)
	}
	req := model.Respond{
		Command:     model.Command{Call: t.call(), Operation: t.OperationID(key + strconv.Itoa(n))},
		Interaction: in.Ref, Mode: mode, Audience: audience, Content: content,
		FollowUp: followUp, ExpiresAt: expires,
	}
	var a model.Acceptance
	err := t.bot.resubmit(ctx, func() (err error) {
		a, err = t.bot.cfg.Client.Interactions().Respond(ctx, req)
		return err
	})
	if !followUp && model.IsCode(err, model.Conflict) {
		err = &claimedError{err}
	}
	return a, err
}
