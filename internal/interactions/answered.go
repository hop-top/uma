package interactions

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// answered is an interaction as an effector loads it for one attempt: the
// record and the response token the store keeps beside it. The token lives
// in this value alone; it is handed to the adapter in the effect and never
// stored, logged or put in an error.
type answered struct {
	rec   model.Interaction
	token store.ResponseToken
}

// loadAnswered reads the interaction ref names and its token. A token the
// store holds none of, such as a provider's lack of one, is empty.
func loadAnswered(ctx context.Context, r store.Reader, ref model.InteractionRef) (answered, error) {
	var a answered
	var err error
	if a.rec, err = r.Interaction(ctx, ref); err != nil {
		return a, err
	}
	a.token, err = r.InteractionToken(ctx, ref)
	if model.IsCode(err, model.NotFound) {
		err = nil
	}
	return a, err
}

// context is the interaction context the adapter's effect carries: the
// record's identifiers, the provider interaction ID, the token and the
// respond window.
func (a answered) context() *adapter.InteractionContext {
	ic := adapter.InteractionContext{
		Ref: a.rec.Ref, Kind: a.rec.Kind, Token: a.token.Token, ExpiresAt: a.rec.RespondUntil,
	}
	for _, x := range a.rec.External {
		if x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindInteraction {
			ic.ProviderID = x.ID
			break
		}
	}
	if a.rec.MessageExternal != nil {
		m := *a.rec.MessageExternal
		ic.Message = &m
	}
	return &ic
}

// recheck refuses an attempt on a redacted interaction (permanent: privacy
// wins over a queued response), at or after the response's own deadline,
// or once the provider's respond window or the token is gone (all
// deadline_expired, final).
func (a answered) recheck(now, deadline time.Time) error {
	switch {
	case a.rec.Redacted:
		return &model.Error{Code: model.Permanent, SafeMessage: "interaction redacted before delivery", Retry: model.NoRetry}
	case !now.Before(deadline):
		return &model.Error{Code: model.DeadlineExpired, SafeMessage: "deadline passed before delivery", Retry: model.NoRetry}
	case !a.rec.RespondUntil.IsZero() && !now.Before(a.rec.RespondUntil):
		return &model.Error{Code: model.DeadlineExpired, SafeMessage: "respond window closed before delivery", Retry: model.NoRetry}
	case a.token.Token == "" && !a.token.ExpiresAt.IsZero():
		return &model.Error{Code: model.DeadlineExpired, SafeMessage: "response token no longer available", Retry: model.NoRetry}
	}
	return nil
}

// recheckResponse reauthorizes the originating actor to respond to the
// interaction under its audience, then runs recheck against deadline.
func (a answered) recheckResponse(ctx context.Context, actor *engine.Client, now, deadline time.Time) error {
	if err := a.recheck(now, deadline); err != nil {
		return err
	}
	return actor.Authorize(ctx, authorizeRespond(a.rec), a.rec.Ref.Ref())
}

// authorizeRespond is the interaction.respond authorization on rec under
// its own audience.
func authorizeRespond(rec model.Interaction) policy.Authorization {
	return policy.Authorization{
		Action: model.ActionInteractionRespond, Resource: rec.Ref.Ref(),
		Audience: engine.ReadAudience(rec.Audience), Revision: rec.Revision,
	}
}

// claimResponse records one accepted response of in by op in the claiming
// unit: the single initial response, or a follow-up, under revision rev. A
// redacted interaction or a second initial response is model.Conflict, the
// unit rolls back. It then queues interaction.updated under the
// interaction's audience, as actor, with event ID id. The event names the
// change and the operation, never content or the token.
func claimResponse(ctx context.Context, tx store.Tx, e *engine.Engine, actor model.ActorID, corr model.CorrelationID,
	ref model.InteractionRef, op model.OperationRef, followUp bool, rev model.Revision, id model.ID,
) error {
	cur, err := tx.Interaction(ctx, ref)
	if err != nil {
		return err
	}
	if cur.Redacted {
		return model.NewError(model.Conflict, "interaction redacted")
	}
	next, err := tx.RecordInteractionResponse(ctx, store.ResponseClaim{Interaction: ref, Operation: op, FollowUp: followUp, Revision: rev})
	if err != nil {
		return err
	}
	change := model.InteractionChangeResponded
	if followUp {
		change = model.InteractionChangeFollowUp
	}
	data, err := json.Marshal(model.InteractionUpdatedPayload{
		Interaction: ref, Change: change, Trigger: model.TriggerCommand, PreviousRevision: cur.Revision, Operation: &op,
	})
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := e.Clock().Now().UTC()
	res := ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventInteractionUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: actor,
		OccurredAt: now, RecordedAt: now, Correlation: corr,
		Audience: engine.ReadAudience(cur.Audience), Revision: next.Revision,
		Payload: model.Extension{Name: model.SchemaInteractionUpdated, Version: model.PayloadVersion1, Data: data},
	}})
}

// precheckResponse refuses a new response, inside its claiming unit, on a
// redacted interaction (conflict), on one whose respond window is gone or
// whose own deadline has passed (deadline_expired). A replay never runs it:
// an accepted response stays replayable after its window.
func precheckResponse(ctx context.Context, tx store.Tx, e *engine.Engine, ref model.InteractionRef, deadline time.Time) error {
	cur, err := tx.Interaction(ctx, ref)
	if err != nil {
		return err
	}
	now := e.Clock().Now()
	switch {
	case cur.Redacted:
		return model.NewError(model.Conflict, "interaction redacted")
	case !now.Before(deadline), !cur.RespondUntil.IsZero() && !now.Before(cur.RespondUntil):
		return expiredError()
	}
	return nil
}
