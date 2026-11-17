package runtime_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// A receipt is authorized as message.receipt on the message at the call
// and again at execution, sent once with its kind on the message's
// provider message, and settles without changing the message.
func TestReceiptSentOnceAndRecorded(t *testing.T) {
	r := newRig(t, enginetest.WithReceipts)
	m := r.SeedMessage("hello")
	a, err := r.Receipt("op-receipt", m, model.ReceiptRead)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	if got := r.Policy.Actions(); len(got) == 0 || got[len(got)-1] != model.ActionMessageReceipt {
		t.Fatalf("policy checks at the call = %v, want message.receipt last", got)
	}
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	calls := r.F.MutationCalls()
	if len(calls) != 1 || calls[0].Capability != model.CapabilityReceipt || calls[0].Receipt != model.ReceiptRead ||
		calls[0].Effect.Target == nil || calls[0].Effect.Target.ID != enginetest.Target {
		t.Fatalf("provider calls = %+v, want one read receipt on %s", calls, enginetest.Target)
	}
	if got := r.Policy.Actions(); countOf(got, model.ActionMessageReceipt) != 2 {
		t.Fatalf("policy checks = %v, want message.receipt at the call and again at execution", got)
	}
	if got := r.Message(m.Ref); got.Revision != m.Revision {
		t.Fatalf("message revision %s after the receipt, want %s", got.Revision, m.Revision)
	}
	if again, err := r.Receipt("op-receipt", m, model.ReceiptRead); err != nil || !again.Replayed {
		t.Fatalf("retry = %+v, %v; want the stored operation", again, err)
	}
	r.Step("w")
	if n := len(r.F.MutationCalls()); n != 1 {
		t.Fatalf("provider calls after the retry = %d, want 1", n)
	}
}

// A denied caller is refused at the call with nothing queued; a permission
// revoked after the call makes no provider call.
func TestReceiptAuthorization(t *testing.T) {
	r := newRig(t, enginetest.WithReceipts)
	m := r.SeedMessage("hello")
	r.Policy.Set(true)
	_, err := r.Receipt("op-denied", m, model.ReceiptRead)
	requireCode(t, err, model.Unauthorized, "")
	r.Policy.Set(false)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("a refused receipt queued %d items", n)
	}
	a, err := r.Receipt("op-revoked", m, model.ReceiptDelivered)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	r.Policy.Set(true)
	r.Step("w")
	r.Policy.Set(false)
	requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(r.F.MutationCalls()); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
}

// A lost answer is reconciled with the receipt's kind before anything is
// sent again; the reconciled receipt settles without a second send.
func TestReceiptLostAnswerIsReconciled(t *testing.T) {
	r := newRig(t, enginetest.WithReceipts, withReconciler)
	m := r.SeedMessage("hello")
	r.F.Respond(lostAnswer)
	a, err := r.Receipt("op-receipt", m, model.ReceiptRead)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	r.Step("w")
	r.Clock.Advance(time.Minute)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"receipt", "reconcile"}) {
		t.Fatalf("calls = %v, want the receipt once, then reconciled", got)
	}
	rc := r.F.Reconciled()
	if len(rc) != 1 || rc[0].Capability != model.CapabilityReceipt || rc[0].Receipt != model.ReceiptRead || rc[0].Target == nil {
		t.Fatalf("reconcile = %+v, want the receipt's capability, kind and target", rc)
	}
}

// A message linked as the echo of a uma send after the call is no received
// message: the attempt fails for good without a provider call.
func TestReceiptForAnEchoFailsWithoutCall(t *testing.T) {
	r := newRig(t, enginetest.WithReceipts)
	m := r.SeedMessage("hello")
	a, err := r.Receipt("op-receipt", m, model.ReceiptRead)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	sent := model.MessageRef{Conversation: m.Ref.Conversation, ID: "00000005-0000-7000-8000-0000000000aa"}
	r.Mutate(m.Ref, func(x *model.Message) { x.EchoOf = &sent })
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Validation {
		t.Fatalf("receipt for an echo = %+v, want failed (validation)", op)
	}
	if n := len(r.F.MutationCalls()); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
}

func countOf(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}
