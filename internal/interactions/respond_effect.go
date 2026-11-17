package interactions

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// ResponseEffector executes interaction response operations.
func ResponseEffector() runtime.Effector { return responseEffector{} }

type responseEffector struct{}

func (responseEffector) Intent() string { return intentResponse }

func (responseEffector) Decode(e model.Extension) (runtime.Job, error) {
	in, err := decodeResponseIntent(e)
	if err != nil {
		return nil, err
	}
	return &responseJob{in: in}, nil
}

// responseJob delivers one interaction response before its deadline. It
// loads the response token with the interaction at each attempt and keeps
// it in memory only, for the effect.
type responseJob struct {
	in  responseIntent
	rt  route.Route
	ans answered
	e   *engine.Engine
}

func (j *responseJob) Audience() model.Audience { return j.in.Audience }

func (j *responseJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.rt, err = route.Load(ctx, r, j.in.Interaction.Conversation); err != nil {
			return err
		}
		j.ans, err = loadAnswered(ctx, r, j.in.Interaction)
		return err
	})
	return j.rt.Channel, err
}

// Route is the conversation's stored route and, for an update, the
// provider message the control sits on.
func (j *responseJob) Route() (model.ExternalRef, *model.ExternalRef) {
	route := j.rt.Conv.ReplyRoute
	if j.in.Mode != model.ResponseModeUpdate || j.ans.rec.MessageExternal == nil {
		return route, nil
	}
	target := *j.ans.rec.MessageExternal
	return route, &target
}

// Interaction is the context the effect answers, with the token loaded for
// this attempt.
func (j *responseJob) Interaction() *adapter.InteractionContext { return j.ans.context() }

// Recheck reloads the interaction and its token, so a redaction or sweep
// since Load is seen, refuses an attempt on a redacted interaction, at or
// after the deadline or past the respond window, then revalidates the
// route and reauthorizes the originating actor to respond.
func (j *responseJob) Recheck(ctx context.Context, actor *engine.Client, _ store.OperationRecord) error {
	if err := route.Check(j.rt); err != nil {
		return err
	}
	var err error
	if j.ans, err = reload(ctx, j.e, j.in.Interaction); err != nil {
		return err
	}
	return j.ans.recheckResponse(ctx, actor, j.e.Clock().Now(), j.in.ExpiresAt)
}

// reload reads the interaction and its token again.
func reload(ctx context.Context, e *engine.Engine, ref model.InteractionRef) (answered, error) {
	var a answered
	err := e.Store().View(ctx, ref.Scope(), func(r store.Reader) (err error) {
		a, err = loadAnswered(ctx, r, ref)
		return err
	})
	return a, err
}

func (j *responseJob) SentContent() model.Content { return j.in.Content.Clone() }

func (j *responseJob) Capabilities() []model.Capability {
	if j.in.FollowUp {
		return []model.Capability{model.CapabilityRespond, model.CapabilityRespondFollowUp}
	}
	return []model.Capability{model.CapabilityRespond}
}

func (j *responseJob) Bind(inst adapter.Instance) (runtime.ProviderCall, bool) {
	r, ok := inst.(adapter.InteractionResponder)
	if !ok {
		return nil, false
	}
	content := j.in.Content.Clone()
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return r.Respond(ctx, adapter.RespondRequest{Effect: eff, Mode: j.in.Mode, Content: content, FollowUp: j.in.FollowUp})
	}, true
}

// Settle records the provider message IDs the settled response reported on
// the message it committed, as a reply does.
func (j *responseJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	return operations.RecordMessageReferences(ctx, tx, j.e, op)
}
