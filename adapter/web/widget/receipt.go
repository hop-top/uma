package widget

import (
	"context"
	"encoding/hex"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// SendReceipt tells the visitor that the host received (delivered) or read
// a message the visitor sent. The receipt is a receipt event in the route's
// visitor log, naming the visitor's message and the kind; it adds no
// reference. Both receipt kinds are expressible; any other kind is
// refused as unsupported.
func (x *Instance) SendReceipt(ctx context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	if r.Kind != model.ReceiptDelivered && r.Kind != model.ReceiptRead {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Unsupported, SafeMessage: "widget has no such receipt", Retry: model.NoRetry,
		}}, nil
	}
	return x.mutateTarget(ctx, r.Effect, Event{Type: EventReceipt, Kind: r.Kind}, x.receiptTarget)
}

// receiptTarget returns the widget message ID t names: a provider message
// reference of this channel with the shape of an ID the receiver gave a
// message a visitor sent. A receipt answers a received message only.
func (x *Instance) receiptTarget(t *model.ExternalRef) (string, error) {
	if t == nil || *t != MessageRef(x.ref(), t.ID) || !isVisitorMessageID(t.ID) {
		return "", model.NewError(model.Validation, "target is not a visitor message of this channel")
	}
	return t.ID, nil
}

// isVisitorMessageID reports whether id has the shape of the ID of a
// message a visitor sent: the prefix 'v' and 16 hashed bytes in hex.
func isVisitorMessageID(id string) bool {
	if len(id) != 33 || id[0] != 'v' {
		return false
	}
	_, err := hex.DecodeString(id[1:])
	return err == nil
}

var _ adapter.ReceiptSender = (*Instance)(nil)
