package interactions

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/route"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// EphemeralEffector executes ephemeral reply operations.
func EphemeralEffector() runtime.Effector { return ephemeralEffector{} }

type ephemeralEffector struct{}

func (ephemeralEffector) Intent() string { return intentEphemeral }

func (ephemeralEffector) Decode(e model.Extension) (runtime.Job, error) {
	in, err := decodeEphemeralIntent(e)
	if err != nil {
		return nil, err
	}
	return &ephemeralJob{in: in}, nil
}

// ephemeralJob delivers an ephemeral reply to its recipient before its
// deadline.
type ephemeralJob struct {
	in ephemeralIntent
	rt route.Route
	to model.ExternalRef
	e  *engine.Engine
	// ans is the interaction a follow-up answers, nil for an independent
	// reply.
	ans *answered
}

func (j *ephemeralJob) Audience() model.Audience { return ephemeralAudience() }

func (j *ephemeralJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.rt, err = route.Load(ctx, r, j.in.Conversation); err != nil {
			return err
		}
		if j.in.Interaction != nil {
			a, err := loadAnswered(ctx, r, *j.in.Interaction)
			if err != nil {
				return err
			}
			j.ans = &a
			id, err := r.GetIdentity(ctx, j.in.Recipient)
			if err != nil {
				return &model.Error{Code: model.Permanent, SafeMessage: "recipient no longer stored", Retry: model.NoRetry}
			}
			j.to = recipientRef(j.rt.Inbox.Channel, id)
			return nil
		}
		id, err := recipient(ctx, r, j.rt.Conv, j.in.Recipient)
		if err != nil {
			return &model.Error{Code: model.Permanent, SafeMessage: "recipient no longer a participant", Retry: model.NoRetry}
		}
		j.to = recipientRef(j.rt.Inbox.Channel, id)
		return nil
	})
	return j.rt.Channel, err
}

// Interaction is the interaction a follow-up answers, with the token
// loaded for this attempt; nil for an independent reply.
func (j *ephemeralJob) Interaction() *adapter.InteractionContext {
	if j.ans == nil {
		return nil
	}
	return j.ans.context()
}

func (j *ephemeralJob) Route() (model.ExternalRef, *model.ExternalRef) {
	return j.rt.Conv.ReplyRoute, nil
}

// Recheck refuses an attempt at or after the deadline, then revalidates
// the route and reauthorizes the originating actor. A follow-up first
// reloads its interaction and token, so a redaction since Load is seen,
// and is refused once redacted or past the respond window; the actor also
// needs interaction.respond.
func (j *ephemeralJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	now := j.e.Clock().Now()
	if j.ans == nil {
		if !now.Before(j.in.ExpiresAt) {
			return &model.Error{Code: model.DeadlineExpired, SafeMessage: "deadline passed before delivery", Retry: model.NoRetry}
		}
		return route.Recheck(ctx, actor, rec, j.rt, ephemeralAudience())
	}
	a, err := reload(ctx, j.e, *j.in.Interaction)
	if err != nil {
		return err
	}
	*j.ans = a
	if err := j.ans.recheckResponse(ctx, actor, now, j.in.ExpiresAt); err != nil {
		return err
	}
	return route.Recheck(ctx, actor, rec, j.rt, ephemeralAudience())
}

func (j *ephemeralJob) SentContent() model.Content { return j.in.Content.Clone() }

func (j *ephemeralJob) Capabilities() []model.Capability {
	if j.in.Interaction != nil {
		return []model.Capability{model.CapabilitySendEphemeral, model.CapabilityRespondFollowUp}
	}
	return []model.Capability{model.CapabilitySendEphemeral}
}

func (j *ephemeralJob) Bind(inst adapter.Instance) (runtime.ProviderCall, bool) {
	s, ok := inst.(adapter.EphemeralSender)
	if !ok {
		return nil, false
	}
	content := j.in.Content.Clone()
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return s.SendEphemeral(ctx, adapter.EphemeralRequest{
			SendRequest: adapter.SendRequest{Effect: eff, Content: content}, Recipient: j.to, ExpiresAt: j.in.ExpiresAt,
		})
	}, true
}
