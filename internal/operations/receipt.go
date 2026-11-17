package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// IntentReceipt names the stored intent of a receipt operation.
const IntentReceipt = "uma.intent.receipt"

// ReceiptIntent is the durable intent of a receipt operation: the shared
// mutation part and the receipt kind.
type ReceiptIntent struct {
	MutationIntent
	Kind string `json:"kind"`
}

// Receipt queues a delivery or read receipt for a received message under
// model.CapabilityReceipt and model.ActionMessageReceipt, with Edit's
// target, authorization and result rules, except that ExpectedRevision is
// absent and the target must be a received message (Received). Nothing in
// uma changes when the provider applies it.
func Receipt(ctx context.Context, c *engine.Client, req model.Receipt) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	return submitMutation(ctx, c, mutation{
		kind: model.ActionMessageReceipt, capability: model.CapabilityReceipt,
		message: req.Message, command: req.Command,
		fingerprint: func(model.Audience) []byte { return receiptFingerprint(req) },
		encode: func(m MutationIntent) (model.Extension, error) {
			return encodeIntent(IntentReceipt, ReceiptIntent{MutationIntent: m, Kind: req.Kind})
		},
		eligible: Received,
	})
}

// Received refuses, with model.Validation, a receipt for a message that
// did not arrive from the provider: one sent through uma (it has an
// Origin) or the provider's echo of one (EchoOf).
func Received(m model.Message) error {
	if m.Origin != nil || m.EchoOf != nil {
		return model.NewError(model.Validation, "message: a receipt answers a received message only")
	}
	return nil
}

// receiptFingerprint returns the version 1 fingerprint of a receipt
// operation: kind, target message and receipt kind.
func receiptFingerprint(req model.Receipt) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionMessageReceipt))
	f.Set("target", Field().Ref(req.Message.Ref()))
	f.Set("receipt", Field().Str(req.Kind))
	return f.Sum()
}

// DecodeReceiptIntent reads a stored receipt intent.
func DecodeReceiptIntent(e model.Extension) (ReceiptIntent, error) {
	return decodeIntent[ReceiptIntent](e, IntentReceipt)
}
