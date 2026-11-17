package runtime_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

func heldProposal(t *testing.T, r *rig, id model.OperationID, ttl time.Duration) model.Operation {
	t.Helper()
	c, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	conv := r.Conv.Ref.Ref()
	exp := r.Clock.Now().Add(ttl).UTC()
	a, err := operations.Submit(r.Ctx, c.Client, operations.Submission{
		Kind: model.ActionMessageReply, Target: conv, Owner: conv,
		Command:     model.Command{Call: model.Call{Correlation: "c"}, Operation: id},
		Fingerprint: []byte("fp-" + id),
		Intent:      model.Extension{Name: "test.intent", Version: 1, Data: []byte(`{}`)},
		Hold:        &operations.Hold{Reason: "r", TrustKey: "k", ExpiresAt: &exp},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return a.Operation
}

// Run sweeps on its cadence, judged by the engine clock: a held operation
// is marked carried over after one window and expired past its deadline.
func TestRunSweepsHeldOnCadence(t *testing.T) {
	r := newRig(t)
	held := heldProposal(t, r, "p-1", time.Hour)
	const every = 5 * time.Minute
	done := make(chan error, 1)
	go func() {
		done <- r.RT.Run(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w", SweepEvery: every})
	}()
	w := waiterOf(r)
	step := func(advance time.Duration) {
		t.Helper()
		<-w.Idle // the loop reached its idle wait: its sweep ran
		r.Clock.Advance(advance)
		w.Resume <- struct{}{}
	}
	step(every / 2) // first sweep at start: nothing due
	if op := r.Op(held.Ref); op.State != model.Held || op.Held.CarriedOverAt != nil {
		t.Fatalf("first sweep: %s %+v", op.State, op.Held)
	}
	step(every) // a window held, cadence reached
	<-w.Idle
	if op := r.Op(held.Ref); op.State != model.Held || op.Held.CarriedOverAt == nil {
		t.Fatalf("carry-over: %s %+v", op.State, op.Held)
	}
	r.Clock.Advance(time.Hour)
	w.Resume <- struct{}{}
	<-w.Idle
	if op := r.Op(held.Ref); op.State != model.Expired || op.Held.Decision != model.HeldExpired {
		t.Fatalf("expiry: %s %+v", op.State, op.Held)
	}
	if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunSweepOffLeavesHeld(t *testing.T) {
	r := newRig(t)
	held := heldProposal(t, r, "p-1", time.Minute)
	r.Clock.Advance(time.Hour)
	done := make(chan error, 1)
	go func() {
		done <- r.RT.Run(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w", SweepEvery: -1})
	}()
	<-waiterOf(r).Idle
	if op := r.Op(held.Ref); op.State != model.Held {
		t.Fatalf("state = %s, want held with the sweep off", op.State)
	}
	if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
