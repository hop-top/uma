package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// membershipEffector executes membership operations through the adapter's
// MembershipChanger.
type membershipEffector struct{ e *engine.Engine }

func (membershipEffector) Intent() string { return operations.IntentMembership }

func (fx membershipEffector) Decode(x model.Extension) (Job, error) {
	in, err := operations.DecodeMembershipIntent(x)
	if err != nil {
		return nil, err
	}
	return &membershipJob{e: fx.e, in: in}, nil
}

// membershipJob adds or removes one participant of the conversation at the
// provider, on the conversation's stored route.
type membershipJob struct {
	e  *engine.Engine
	in operations.MembershipIntent
	rt route.Route
	// identity is the participant as last read; nil when it is gone.
	identity *model.Identity
}

func (j *membershipJob) Audience() model.Audience { return operations.MembershipAudience() }

func (j *membershipJob) Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityMembership}
}

// Load reads the conversation's route and the identity.
func (j *membershipJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.rt, err = route.Load(ctx, r, j.in.Conversation); err != nil {
			return err
		}
		return j.loadIdentity(ctx, r)
	})
	return j.rt.Channel, err
}

func (j *membershipJob) loadIdentity(ctx context.Context, r store.Reader) error {
	id, err := operations.MembershipIdentity(ctx, r, j.in.Identity)
	switch {
	case model.IsCode(err, model.NotFound):
		j.identity = nil
		return nil
	case err != nil:
		return err
	}
	j.identity = &id
	return nil
}

// Route is the conversation's stored route; the effect has no target.
func (j *membershipJob) Route() (model.ExternalRef, *model.ExternalRef) {
	return j.rt.Conv.ReplyRoute, nil
}

// Recheck revalidates the route, requires the identity still stored and
// reauthorizes the originating actor under the operation's action on the
// conversation.
func (j *membershipJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	err := j.e.Store().View(ctx, j.in.Conversation.Inbox.Tenant, func(r store.Reader) error { return j.loadIdentity(ctx, r) })
	if err != nil {
		return err
	}
	if j.identity == nil {
		return &model.Error{Code: model.NotFound, SafeMessage: "participant identity no longer stored", Retry: model.NoRetry}
	}
	return route.Recheck(ctx, actor, rec, j.rt, j.Audience())
}

// MembershipChange is the participant an attempt adds or removes, handed
// to Reconcile with the attempt.
func (j *membershipJob) MembershipChange() (model.ExternalRef, bool) {
	if j.identity == nil {
		return model.ExternalRef{}, j.in.Remove
	}
	return operations.ParticipantRef(j.rt.Inbox.Channel, *j.identity), j.in.Remove
}

func (j *membershipJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	mc, ok := inst.(adapter.MembershipChanger)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		participant, remove := j.MembershipChange()
		return mc.ChangeMembership(ctx, adapter.MembershipRequest{Effect: eff, Participant: participant, Remove: remove})
	}, true
}

// Settle records the membership change once the provider applied it (see
// operations.SettleMembership).
func (j *membershipJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	return operations.SettleMembership(ctx, tx, j.e, op, j.in)
}
