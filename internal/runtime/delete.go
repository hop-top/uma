package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// deleteEffector executes delete operations through the adapter's
// Deleter.
type deleteEffector struct{ e *engine.Engine }

func (deleteEffector) Intent() string { return operations.IntentDelete }

func (fx deleteEffector) Decode(x model.Extension) (Job, error) {
	in, err := operations.DecodeDeleteIntent(x)
	if err != nil {
		return nil, err
	}
	return &deleteJob{mutationJob: mutationJob{e: fx.e, in: in.MutationIntent, capability: model.CapabilityDelete}, intent: in}, nil
}

// deleteJob is one delete of the target.
type deleteJob struct {
	mutationJob
	intent operations.DeleteIntent
}

func (j *deleteJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	d, ok := inst.(adapter.Deleter)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return d.Delete(ctx, adapter.DeleteRequest{Effect: eff})
	}, true
}

// Settle tombstones the target once the provider applied the delete (see
// operations.SettleDelete).
func (j *deleteJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	return operations.SettleDelete(ctx, tx, j.e, op, j.intent)
}
