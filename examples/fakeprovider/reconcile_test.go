package fakeprovider_test

import (
	"context"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

func (h *host) reconcile(c uma.Client, ref model.OperationRef) (model.Operation, error) {
	return c.Operations().Reconcile(h.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "reconcile"}, Ref: ref})
}

func methods(calls []fakeprovider.Call) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = string(c.Method)
	}
	return out
}

// A host that sees a delivery stuck in outcome_unknown asks for its
// reconciliation instead of waiting for the next run: the provider's record
// settles it, nothing is sent twice, and the reconciliation the runtime had
// scheduled finds it settled.
func TestReconcileMovesLostAckDeliveryToTerminalState(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	h.factory.Script(chanA).Respond(fakeprovider.OutcomeUnknown)
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	stuck := h.op(ref)
	if stuck.State != model.Unknown || stuck.NextAttemptAt == nil {
		t.Fatalf("op = %+v, want outcome_unknown with its next attempt time", stuck)
	}
	op, err := h.reconcile(h.client, ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if op.State != model.ProviderAccepted || op.Failure != nil || op.NextAttemptAt != nil || len(op.ProviderReferences) != 1 {
		t.Fatalf("op = %+v, want provider_accepted with the provider's reference", op)
	}
	again, err := h.reconcile(h.client, ref)
	if err != nil || again.Revision != op.Revision {
		t.Fatalf("second Reconcile = %+v, %v; want the settled operation unchanged", again, err)
	}
	h.later()
	h.stop()
	if got := methods(h.factory.Script(chanA).Calls()); len(got) != 2 || got[0] != string(fakeprovider.MethodSend) || got[1] != string(fakeprovider.MethodReconcile) {
		t.Fatalf("provider calls = %v, want the send and the one reconcile", got)
	}
}

// A request the provider never received leaves no record: Reconcile shows
// the send was not applied, sends nothing itself, and the runtime then
// sends it once.
func TestReconcileOfLostRequestSchedulesTheRetry(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	h.factory.Script(chanA).Respond(fakeprovider.RequestLost)
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	op, err := h.reconcile(h.client, ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if op.State != model.RetryPending || op.NextAttemptAt == nil {
		t.Fatalf("op = %+v, want retry_pending with the retry scheduled", op)
	}
	if n := sends(h.factory.Script(chanA).Calls()); n != 1 {
		t.Fatalf("sends = %d after Reconcile, want the lost one only", n)
	}
	h.later()
	h.stop()
	if got := h.op(ref); got.State != model.ProviderAccepted {
		t.Fatalf("op = %+v, want the retry accepted", got)
	}
	if n := sends(h.factory.Script(chanA).Calls()); n != 2 {
		t.Fatalf("sends = %d, want the lost one and one retry", n)
	}
}

// Callers without operation.reconcile are refused and the provider is not
// asked.
func TestReconcileRefusesUnauthorizedCaller(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	h.factory.Script(chanA).Respond(fakeprovider.OutcomeUnknown)
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	engine, err := uma.New(uma.Config{
		Store: h.store,
		Authorizer: policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: a.Action != model.ActionOperationReconcile}, nil
		}),
		Adapters: []adapter.Factory{h.factory}, Extensions: h.factory.Extensions(), Clock: h.clock, IDs: portstest.NewSequenceIDs(0x77),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	reader, err := engine.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcile(reader, ref); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("Reconcile without operation.reconcile = %v, want unauthorized", err)
	}
	if got := h.op(ref); got.State != model.Unknown {
		t.Fatalf("op = %+v, want it untouched", got)
	}
	if n := len(h.factory.Script(chanA).Calls()); n != 1 {
		t.Fatalf("provider calls = %d, want only the send", n)
	}
	h.stop()
}
