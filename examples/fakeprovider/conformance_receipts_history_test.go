package fakeprovider_test

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
)

// The receipt and history side of the conformance provider: a variant
// without delivered receipts, and the account's history.

// HoldHistory holds ev in the account's history as the envelope Deliver
// reports for it, occurring at at.
func (p *provider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	if ev.Kind != conformance.InboundMessage {
		return conformance.ErrNotExpressible
	}
	s, acct := p.script(ch)
	env, err := p.envelope(acct, ev)
	if err != nil {
		return err
	}
	env.OccurredAt = at
	s.HoldHistory(env)
	return nil
}

var _ conformance.HistoryProvider = (*provider)(nil)

// readReceiptsFactory is the full variant whose provider has no delivered
// receipt: the adapter refuses one before any provider call.
type readReceiptsFactory struct{ *fakeprovider.Factory }

func (f readReceiptsFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return readReceiptsInstance{inst.(*fakeprovider.Full)}, nil
}

type readReceiptsInstance struct{ *fakeprovider.Full }

func (i readReceiptsInstance) SendReceipt(ctx context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	if r.Kind != model.ReceiptRead {
		return adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Unsupported, "fake provider: no "+r.Kind+" receipt")}, nil
	}
	return i.Full.SendReceipt(ctx, r)
}
