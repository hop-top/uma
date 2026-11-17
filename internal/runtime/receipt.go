package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// receiptEffector executes receipt operations through the adapter's
// ReceiptSender.
type receiptEffector struct{ e *engine.Engine }

func (receiptEffector) Intent() string { return operations.IntentReceipt }

func (fx receiptEffector) Decode(x model.Extension) (Job, error) {
	in, err := operations.DecodeReceiptIntent(x)
	if err != nil {
		return nil, err
	}
	return &receiptJob{mutationJob: mutationJob{e: fx.e, in: in.MutationIntent, capability: model.CapabilityReceipt}, intent: in}, nil
}

// receiptJob sends one receipt for the target. It settles nothing in uma:
// the operation is the record that the receipt was sent.
type receiptJob struct {
	mutationJob
	intent operations.ReceiptIntent
}

// ReceiptKind is the receipt kind, handed to Reconcile with the attempt.
func (j *receiptJob) ReceiptKind() string { return j.intent.Kind }

// Recheck adds to the mutation recheck that the target is still a received
// message: a message linked as the echo of a uma send since the call is
// not one.
func (j *receiptJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	if err := j.mutationJob.Recheck(ctx, actor, rec); err != nil {
		return err
	}
	if err := operations.Received(*j.msg); err != nil {
		return noRetry(AsFailure(err))
	}
	return nil
}

func (j *receiptJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	rs, ok := inst.(adapter.ReceiptSender)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return rs.SendReceipt(ctx, adapter.ReceiptRequest{Effect: eff, Kind: j.intent.Kind})
	}, true
}
