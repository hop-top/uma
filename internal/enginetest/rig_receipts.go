package enginetest

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// WithReceipts declares the receipt capability.
func WithReceipts(_ *engine.Config, f *OutFactory) { f.Receipts = true }

// SendReceipt answers like a send, consuming the same script.
func (i *OutInstance) SendReceipt(ctx context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	return i.mutate(ctx, MutationCall{Capability: model.CapabilityReceipt, Effect: r.Effect, Receipt: r.Kind})
}

// Receipt submits a receipt of kind for m.
func (r *Rig) Receipt(opID model.OperationID, m model.Message, kind string) (model.Acceptance, error) {
	return r.C.Messages().Receipt(r.Ctx, model.Receipt{
		Command: model.Command{Call: model.Call{Correlation: "corr-receipt"}, Operation: opID},
		Message: m.Ref, Kind: kind,
	})
}
