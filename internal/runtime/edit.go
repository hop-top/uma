package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// editEffector executes edit operations through the adapter's Editor.
type editEffector struct{ e *engine.Engine }

func (editEffector) Intent() string { return operations.IntentEdit }

func (fx editEffector) Decode(x model.Extension) (Job, error) {
	in, err := operations.DecodeEditIntent(x)
	if err != nil {
		return nil, err
	}
	return &editJob{mutationJob: mutationJob{e: fx.e, in: in.MutationIntent, capability: model.CapabilityEdit}, intent: in}, nil
}

// editJob is one edit: the replacement content of the target.
type editJob struct {
	mutationJob
	intent operations.EditIntent
}

func (j *editJob) SentContent() model.Content { return j.intent.Content.Clone() }

func (j *editJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	ed, ok := inst.(adapter.Editor)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return ed.Edit(ctx, adapter.EditRequest{Effect: eff, Content: j.intent.Content.Clone()})
	}, true
}

// Settle applies the edit once the provider applied it, unless the
// target's edit state advanced (see operations.SettleEdit).
func (j *editJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	return operations.SettleEdit(ctx, tx, j.e, op, j.intent)
}
