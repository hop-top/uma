package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// reactEffector executes react operations through the adapter's Reactor.
type reactEffector struct{ e *engine.Engine }

func (reactEffector) Intent() string { return operations.IntentReact }

func (fx reactEffector) Decode(x model.Extension) (Job, error) {
	in, err := operations.DecodeReactIntent(x)
	if err != nil {
		return nil, err
	}
	return &reactJob{mutationJob: mutationJob{e: fx.e, in: in.MutationIntent, capability: model.CapabilityReact}, intent: in}, nil
}

// reactJob adds or removes one reaction of the account on the target.
type reactJob struct {
	mutationJob
	intent operations.ReactIntent
}

func (j *reactJob) Reaction() (emoji string, remove bool) { return j.intent.Emoji, j.intent.Remove }

func (j *reactJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	rx, ok := inst.(adapter.Reactor)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return rx.React(ctx, adapter.ReactRequest{Effect: eff, Emoji: j.intent.Emoji, Remove: j.intent.Remove})
	}, true
}

// Settle records the caller's reaction change once the provider applied
// it (see operations.SettleReact).
func (j *reactJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	return operations.SettleReact(ctx, tx, j.e, op, j.intent)
}
