package interactions

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// intentResponse names the stored intent of an interaction response.
const intentResponse = "uma.intent.interaction_response"

// responseIntent is the durable intent of an interaction response: what a
// worker needs to deliver it after a restart. It never holds the response
// token, which the effector reads from the interaction's storage at attempt
// time.
type responseIntent struct {
	Interaction model.InteractionRef `json:"interaction"`
	Mode        string               `json:"mode"`
	Audience    model.Audience       `json:"audience"`
	Content     model.Content        `json:"content"`
	FollowUp    bool                 `json:"follow_up"`
	ExpiresAt   time.Time            `json:"expires_at"`
}

func decodeResponseIntent(e model.Extension) (responseIntent, error) {
	var in responseIntent
	if e.Name != intentResponse || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// Respond answers an interaction through the adapter that reported it, in
// the explicit Mode and Audience of the request: message posts a new
// message and commits it with the operation, ephemeral answers the actor
// only (the audience must be restricted) and stores no message, update
// replaces the provider message the control sits on and stores none.
//
// The principal needs interaction.respond on the interaction under its
// audience and the adapter must declare respond, and respond_follow_up for
// a FollowUp. ExpiresAt bounds the attempt and may not pass the
// interaction's RespondUntil when that is set; a response whose window or
// own deadline has passed is model.DeadlineExpired, a redacted interaction
// model.Conflict.
//
// The initial response (not FollowUp) is claimed atomically with the
// operation: of two concurrent initial responses exactly one is accepted
// and the other is model.Conflict. A follow-up counts one and may precede
// the initial response. The same operation ID with the same payload
// replays the accepted response, also after its window; a replay claims
// and counts nothing. The response token is read at the attempt and handed
// to the adapter in the effect; it is never stored in the operation, its
// evidence or events.
func (s Service) Respond(ctx context.Context, req model.Respond) (model.Acceptance, error) {
	e := s.c.Engine()
	if e.IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Interaction.Scope() != s.c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var in model.Interaction
	var rt route.Route
	err := e.Store().View(ctx, s.c.Principal().Tenant, func(r store.Reader) error {
		var err error
		if in, err = r.Interaction(ctx, req.Interaction); err != nil {
			return err
		}
		if rt, err = route.Load(ctx, r, req.Interaction.Conversation); err != nil {
			return err
		}
		caps := []model.Capability{model.CapabilityRespond}
		if req.FollowUp {
			caps = append(caps, model.CapabilityRespondFollowUp)
		}
		return route.Declares(e, rt.Channel.Type, caps...)
	})
	if err != nil {
		return model.Acceptance{}, route.NotFound(err)
	}
	if in.Ref != req.Interaction {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err := checkResponse(e.Clock().Now(), req, in); err != nil {
		return model.Acceptance{}, err
	}
	data, err := json.Marshal(responseIntent{
		Interaction: req.Interaction, Mode: req.Mode, Audience: req.Audience, Content: req.Content.Clone(),
		FollowUp: req.FollowUp, ExpiresAt: req.ExpiresAt.UTC(),
	})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "response content cannot be stored")
	}
	eventID, err := operations.NewID(ctx, e)
	if err != nil {
		return model.Acceptance{}, err
	}
	sub := operations.Submission{
		Kind: model.ActionInteractionRespond, Target: req.Interaction.Ref(), Owner: req.Interaction.Ref(),
		Command: req.Command, Audience: engine.ReadAudience(in.Audience), Fingerprint: responseFingerprint(req),
		Intent: model.Extension{Name: intentResponse, Version: 1, Data: data},
		Precheck: func(ctx context.Context, tx store.Tx) error {
			return precheckResponse(ctx, tx, e, req.Interaction, req.ExpiresAt)
		},
	}
	var msg *operations.ResponseMessage
	if req.Mode == model.ResponseModeMessage {
		if msg, err = operations.NewResponseMessage(ctx, s.c, req.Interaction, req.Correlation, rt.Channel.Type, req.Content, req.Audience); err != nil {
			return model.Acceptance{}, err
		}
		ref, result := msg.Ref(), msg.Ref().Ref()
		sub.ResultMessage, sub.Result = &ref, &result
	}
	msgRev := model.Revision("")
	if msg != nil {
		if msgRev, err = operations.NewRevision(ctx, e); err != nil {
			return model.Acceptance{}, err
		}
	}
	actor := s.c.Principal().Actor
	sub.Apply = func(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
		err := claimResponse(ctx, tx, e, actor, req.Correlation, req.Interaction, op, req.FollowUp, rev, eventID)
		if model.IsCode(err, model.Conflict) && !req.FollowUp {
			return model.NewError(model.Conflict, "interaction already has an initial response")
		}
		if err != nil || msg == nil {
			return err
		}
		return msg.Write(ctx, tx, op, msgRev)
	}
	return submit(ctx, s.c, sub)
}

// checkResponse applies the rules of a response that hold for the request
// and the stored interaction alone: an update needs the provider message
// the control sits on, and the deadline may not pass a respond window that
// is still open. A window already gone is the claiming unit's
// deadline_expired, which a replay of an accepted response never meets.
func checkResponse(now time.Time, req model.Respond, in model.Interaction) error {
	if req.Mode == model.ResponseModeUpdate && in.MessageExternal == nil {
		return model.NewError(model.Validation, "mode: update needs the provider message the control sits on")
	}
	if now.Before(in.RespondUntil) && req.ExpiresAt.After(in.RespondUntil) {
		return model.NewError(model.Validation, "expires_at: must not pass the interaction's respond window")
	}
	return nil
}

// responseFingerprint returns the version 1 fingerprint of an interaction
// response: kind, target interaction, mode, audience, content, expiry and
// whether it is a follow-up.
func responseFingerprint(req model.Respond) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionInteractionRespond))
	f.Set("target", operations.Field().Ref(req.Interaction.Ref()))
	f.Set("mode", operations.Field().Str(req.Mode))
	f.Set("audience", operations.Field().Audience(req.Audience))
	f.Set("content", operations.Field().Content(req.Content))
	f.Set("expires_at", operations.Field().Str(req.ExpiresAt.UTC().Format(time.RFC3339Nano)))
	f.Set("follow_up", operations.Field().Present(req.FollowUp))
	return f.Sum()
}
