package extensions

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Effector returns the runtime effector of extension effects.
func Effector() runtime.Effector { return effector{} }

type effector struct{}

func (effector) Intent() string { return intentName }

func (effector) Decode(e model.Extension) (runtime.Job, error) {
	in, err := decodeIntent(e)
	if err != nil {
		return nil, err
	}
	return &job{intent: in}, nil
}

// job is one extension effect: its intent, the target it loaded and the
// definition its recheck found.
type job struct {
	intent intent
	target target
	def    adapter.ExtensionDefinition
}

func (j *job) Audience() model.Audience { return j.intent.Audience }

// Load resolves the target again: its current channel executes the effect.
func (j *job) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	if j.intent.Target.Tenant != tenant {
		return model.Channel{}, engine.ErrNotFound()
	}
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		j.target, err = resolve(ctx, r, j.intent.Target)
		return err
	})
	return j.target.channel, err
}

func (j *job) Route() (model.ExternalRef, *model.ExternalRef) { return j.target.route, nil }

// Recheck finds the definition the action names in the engine's current
// composition, so an action no longer registered, or registered with
// another kind, never executes, then reauthorizes the originating actor
// under the definition's policy action.
func (j *job) Recheck(ctx context.Context, actor *engine.Client, _ store.OperationRecord) error {
	d, err := definition(actor.Engine(), j.intent.Action, model.ExtensionKindEffect)
	if err != nil {
		return err
	}
	j.def = d
	return actor.Authorize(ctx, policy.Authorization{
		Action: d.AuthorizeAs, Resource: j.intent.Target, Audience: j.intent.Audience,
	}, j.target.owner)
}

func (j *job) Capabilities() []model.Capability {
	return capabilities(model.CapabilityExtensionExecute, j.def)
}

// RequestedAction is the action the attempt carried, for its
// reconciliation (runtime.Executing).
func (j *job) RequestedAction() model.Extension { return j.intent.Action.Clone() }

var _ runtime.Executing = (*job)(nil)

func (j *job) Bind(inst adapter.Instance) (runtime.ProviderCall, bool) {
	x, ok := inst.(adapter.ExtensionExecutor)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return x.Execute(ctx, adapter.ExecuteRequest{Effect: eff, Action: j.intent.Action.Clone()})
	}, true
}
