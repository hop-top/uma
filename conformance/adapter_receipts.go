package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

// The receipt cases send receipts through the engine (Messages.Receipt) for
// a message the adapter received, in the provider's own form: a receipt
// answers a received message, never one uma sent. Every case that scripts
// an answer does so after the message exists.

var allReceiptKinds = []string{model.ReceiptDelivered, model.ReceiptRead}

// receiptKinds returns the receipt kinds the provider sends, or those it
// has none for (AdapterHarness.UnsupportedReceiptKinds). It fails on a
// harness listing an unknown kind or every kind: an adapter with no
// receipt to send declares no receipt.
func (e *adapterEnv) receiptKinds(t *testing.T, supported bool) []string {
	t.Helper()
	for _, k := range e.h.UnsupportedReceiptKinds {
		if !slices.Contains(allReceiptKinds, k) {
			t.Fatalf("AdapterHarness.UnsupportedReceiptKinds lists %q, not a receipt kind", k)
		}
	}
	var out []string
	for _, k := range allReceiptKinds {
		if slices.Contains(e.h.UnsupportedReceiptKinds, k) != supported {
			out = append(out, k)
		}
	}
	if supported && len(out) == 0 {
		t.Fatal("AdapterHarness.UnsupportedReceiptKinds lists every receipt kind, yet receipt is declared")
	}
	return out
}

// receiptTarget receives a message and returns it, with the worker started
// and idle. The message must hold its provider message reference: a
// receipt names it.
func (e *adapterEnv) receiptTarget(t *testing.T) mutationTarget {
	t.Helper()
	g := e.inboundTarget(t, "")
	if g.id == "" {
		t.Fatalf("received message %s holds no provider message reference: %+v", g.msg.Ref.ID, g.msg.External)
	}
	return g
}

func (e *adapterEnv) receipt(m model.Message, id model.OperationID, kind string) (model.Acceptance, error) {
	return e.client.Messages().Receipt(e.ctx, model.Receipt{Command: command(id), Message: m.Ref, Kind: kind})
}

// mustReceipt submits a receipt that must be queued with m untouched.
func (e *adapterEnv) mustReceipt(t *testing.T, m model.Message, id model.OperationID, kind string) model.OperationRef {
	t.Helper()
	a, err := e.receipt(m, id, kind)
	return e.queued(t, a, err, m)
}

// receiptEffect is a receipt of kind for g: uma shows nothing of it,
// applied or not.
func (e *adapterEnv) receiptEffect(g mutationTarget, kind string) queuedEffect {
	unchanged := func(t *testing.T) { t.Helper(); e.wantUnchanged(t, g.msg) }
	return queuedEffect{
		capability: model.CapabilityReceipt,
		submit: func(t *testing.T, id model.OperationID) model.OperationRef {
			t.Helper()
			return e.mustReceipt(t, g.msg, id, kind)
		},
		untouched: unchanged, applied: unchanged,
	}
}

var receiptCap = []model.Capability{model.CapabilityReceipt}

var adapterReceiptCases = []adapterCase{
	{name: "ReceiptTargetsReceivedProviderMessage", needs: receiptCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		kinds := e.receiptKinds(t, true)
		g := e.receiptTarget(t)
		for i, kind := range kinds {
			ref := e.mustReceipt(t, g.msg, model.OperationID("op-receipt-"+kind), kind)
			e.later(t, 0)
			wantActApplied(t, e.op(t, ref))
			calls := e.calls(adapterChanA, model.CapabilityReceipt)
			if len(calls) != i+1 {
				t.Fatalf("provider receipts = %+v, want %d", calls, i+1)
			}
			c := calls[i]
			if !c.Applied || c.Operation != ref || c.Kind != kind {
				t.Fatalf("provider receipt = %+v, want one applied %s receipt of %v", c, kind, ref)
			}
			if c.Target != g.ref() || c.Route != g.conv.ReplyRoute {
				t.Fatalf("receipt target %+v on route %+v, want the stored provider message %+v on %+v", c.Target, c.Route, g.ref(), g.conv.ReplyRoute)
			}
			// A receipt changes nothing in uma once applied.
			e.wantUnchanged(t, g.msg)
			e.wantChanges(t, ref)
		}
		e.stop(t)
	}},
	{name: "ReceiptRetriedSubmitSentOnce", needs: receiptCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		kind := e.receiptKinds(t, true)[0]
		g := e.receiptTarget(t)
		ref := e.mustReceipt(t, g.msg, "op-receipt", kind)
		a, err := e.receipt(g.msg, "op-receipt", kind)
		wantReplayed(t, a, err, ref)
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		a, err = e.receipt(g.msg, "op-receipt", kind)
		wantReplayed(t, a, err, ref)
		wantActApplied(t, a.Operation)
		e.later(t, time.Hour)
		if calls := e.calls(adapterChanA, model.CapabilityReceipt); len(calls) != 1 {
			t.Fatalf("provider receipts = %+v, want one: a retried submission is never sent again", calls)
		}
		e.stop(t)
	}},
	{name: "ReceiptKindWithoutProviderReceiptRefused", needs: receiptCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		kinds := e.receiptKinds(t, false)
		if len(kinds) == 0 {
			t.Skip("the provider sends every receipt kind")
		}
		g := e.receiptTarget(t)
		for _, kind := range kinds {
			ref := e.mustReceipt(t, g.msg, model.OperationID("op-receipt-"+kind), kind)
			e.later(t, 0)
			e.later(t, time.Hour)
			wantRejected(t, e.op(t, ref), model.Unsupported)
			e.wantUnchanged(t, g.msg)
		}
		if calls := e.calls(adapterChanA, model.CapabilityReceipt); len(calls) != 0 {
			t.Fatalf("provider receipts = %+v, want none: a kind the provider has no receipt for never reaches it", calls)
		}
		e.stop(t)
	}},
	{name: "ReceiptRejectedIsFinal", needs: receiptCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		kind := e.receiptKinds(t, true)[0]
		e.rejectedIsFinal(t, e.receiptEffect(e.receiptTarget(t), kind))
	}},
	{name: "ReceiptAnswerLostIsNeverResent", needs: receiptCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		kind := e.receiptKinds(t, true)[0]
		e.answerLostNeverResent(t, e.receiptEffect(e.receiptTarget(t), kind))
	}},
	{name: "ReceiptRevokedPermissionMakesNoCall", needs: receiptCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		kind := e.receiptKinds(t, true)[0]
		e.revokedMakesNoCall(t, e.receiptEffect(e.receiptTarget(t), kind))
	}},
}
