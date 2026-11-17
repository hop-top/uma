package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// Step runs one bounded claim round in the caller's goroutine: at most
// Limit items, executed to their next durable state, counted, with no
// waiting and no worker left behind.
func TestStepRunsOneBoundedRound(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "one")
	b := r.Reply("op-2", "two")
	opts := runtime.WorkerOptions{Owner: "w", Limit: 1}
	n, err := r.RT.Step(r.Ctx, runtimeCall(), opts)
	if err != nil || n != 1 {
		t.Fatalf("Step = %d, %v, want 1 item", n, err)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	requireState(t, r.Op(b.Operation.Ref), model.Queued, "")
	n, err = r.RT.Step(r.Ctx, runtimeCall(), opts)
	if err != nil || n != 1 {
		t.Fatalf("second Step = %d, %v, want 1 item", n, err)
	}
	requireState(t, r.Op(b.Operation.Ref), model.ProviderAccepted, "")
	n, err = r.RT.Step(r.Ctx, runtimeCall(), opts)
	if err != nil || n != 0 {
		t.Fatalf("idle Step = %d, %v, want 0 items", n, err)
	}
}

// An idle Step returns at once instead of pacing a poll, and defaults the
// way Run does.
func TestStepDoesNotWaitWhenIdle(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(r.Ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	n, err := r.RT.Step(ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w", Poll: time.Hour})
	if err != nil || n != 0 {
		t.Fatalf("Step = %d, %v", n, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("idle Step took %v", d)
	}
}

// Step validates like Run.
func TestStepValidatesOptionsAndCaller(t *testing.T) {
	r := newRig(t)
	_, err := r.RT.Step(r.Ctx, runtimeCall(), runtime.WorkerOptions{})
	requireCode(t, err, model.Validation, "owner")
	_, err = r.RT.Step(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w", Kinds: []string{"ingress.process"}})
	requireCode(t, err, model.Unsupported, "")
	_, err = r.RT.Step(r.Ctx, runtime.Call{}, runtime.WorkerOptions{Owner: "w"})
	requireCode(t, err, model.Validation, "")
}

// After Drain a tenant claims nothing new, through Step as through Run.
func TestStepAfterDrainClaimsNothing(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	n, err := r.RT.Step(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w"})
	requireCode(t, err, model.Unavailable, "draining")
	if n != 0 {
		t.Fatalf("Step = %d items after drain", n)
	}
	requireState(t, r.Op(a.Operation.Ref), model.Queued, "")
}

func TestStepAfterCloseFails(t *testing.T) {
	r := newRig(t)
	if err := r.E.Close(r.Ctx); err != nil {
		t.Fatal(err)
	}
	_, err := r.RT.Step(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w"})
	requireCode(t, err, model.Unavailable, "closed")
}

// Drain waits for a Step in flight, as for a Run, and a finished Step
// leaves nothing for Drain to wait on.
func TestDrainWaitsForStepInFlight(t *testing.T) {
	r := newRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock() // a failed assertion must not leave the provider call held
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		close(entered)
		<-release
		return adapter.Outcome{State: model.ProviderAccepted}, nil, true
	})
	a := r.Reply("op-1", "hello")
	stepped := make(chan error, 1)
	go func() {
		_, err := r.RT.Step(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w"})
		stepped <- err
	}()
	<-entered
	drained := make(chan error, 1)
	go func() { drained <- r.RT.Drain(r.Ctx, runtimeCall()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned %v while a Step held work", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-stepped; err != nil {
		t.Fatalf("Step: %v", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
}

func TestDrainAfterFinishedStepReturns(t *testing.T) {
	r := newRig(t)
	r.Reply("op-1", "hello")
	if _, err := r.RT.Step(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.Ctx, 5*time.Second)
	defer cancel()
	if err := r.RT.Drain(ctx, runtimeCall()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}
