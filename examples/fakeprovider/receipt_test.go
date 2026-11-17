package fakeprovider_test

import (
	"testing"

	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A receipt for a received message runs through the worker: the provider
// gets it once with its kind on the message's provider message, the
// operation records it, and neither a retry of the call nor an answer lost
// after the provider applied it sends it again.
func TestRuntimeReceiptSentOnce(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	provider := model.ProviderMessageRef(chanA, "in-1")
	msg := model.Message{
		Ref:      model.MessageRef{Conversation: convRef, ID: "00000000-0000-7000-8000-0000000000e1"},
		Audience: model.Audience{Visibility: model.Public}, External: []model.ExternalRef{provider},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}}, Revision: "m1",
	}
	if err := h.store.Transact(h.ctx, tenant, func(tx store.Tx) error { return tx.PutMessage(h.ctx, msg, "") }); err != nil {
		t.Fatal(err)
	}
	receipt := func(op model.OperationID, kind string) model.Acceptance {
		t.Helper()
		a, err := h.client.Messages().Receipt(h.ctx, model.Receipt{
			Command: model.Command{Call: model.Call{Correlation: "c-receipt"}, Operation: op}, Message: msg.Ref, Kind: kind,
		})
		if err != nil {
			t.Fatalf("Receipt: %v", err)
		}
		return a
	}
	receipts := func(ref model.OperationRef) []fakeprovider.Call {
		var out []fakeprovider.Call
		for _, c := range h.factory.Script(chanA).Calls() {
			if c.Method == fakeprovider.MethodReceipt && c.Effect.Operation == ref {
				out = append(out, c)
			}
		}
		return out
	}

	read := receipt("op-read", model.ReceiptRead)
	h.factory.Script(chanA).Respond(fakeprovider.Accepted, fakeprovider.OutcomeUnknown)
	h.start("worker")
	if op := h.op(read.Operation.Ref); op.State != model.ProviderAccepted || op.Key == nil || op.Key.Kind != model.ActionMessageReceipt {
		t.Fatalf("read receipt = %+v, want provider_accepted under message.receipt", op)
	}
	if again := receipt("op-read", model.ReceiptRead); !again.Replayed || again.Operation.Ref != read.Operation.Ref {
		t.Fatalf("retry = %+v, want the stored operation", again)
	}
	lost := receipt("op-delivered", model.ReceiptDelivered)
	h.later()
	h.later()
	h.stop()
	if op := h.op(lost.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("receipt whose answer was lost = %+v, want reconciled to provider_accepted", op)
	}
	if got := receipts(read.Operation.Ref); len(got) != 1 || !got[0].Applied || got[0].Kind != model.ReceiptRead || *got[0].Effect.Target != provider {
		t.Fatalf("read receipts sent = %+v, want one applied read receipt on %v", got, provider)
	}
	if got := receipts(lost.Operation.Ref); len(got) != 1 || got[0].Kind != model.ReceiptDelivered {
		t.Fatalf("delivered receipts sent = %+v, want one: a reconciled receipt is not sent again", got)
	}
	stored, err := h.client.Messages().Get(h.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "get"}, Ref: msg.Ref})
	if err != nil || stored.Revision != msg.Revision {
		t.Fatalf("message = %+v, %v; want it unchanged by receipts", stored, err)
	}
}
