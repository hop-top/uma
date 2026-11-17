package uma_test

import (
	"context"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store/memory"
)

// newStepEngine composes an engine whose store reads the same system clock
// as the engine, the way a host composes one.
func newStepEngine(t *testing.T) uma.Engine {
	t.Helper()
	clock := ports.SystemClock{}
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Clock: clock,
		IDs:   ports.NewUUIDv7(clock),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

var stepCall = uma.RuntimeCall{Principal: principal, Call: model.Call{Correlation: "step"}}

// Step on an engine with nothing queued returns at once with no items, and
// needs no worker, waiter or poll interval.
func TestRuntimeStepIdleReturnsZero(t *testing.T) {
	e := newStepEngine(t)
	n, err := e.Runtime().Step(context.Background(), stepCall, uma.WorkerOptions{Owner: "w", Limit: 5})
	if err != nil || n != 0 {
		t.Fatalf("Step = %d, %v, want 0, nil", n, err)
	}
}

func TestRuntimeStepValidatesOptions(t *testing.T) {
	e := newStepEngine(t)
	_, err := e.Runtime().Step(context.Background(), stepCall, uma.WorkerOptions{})
	requireCode(t, err, model.Validation, "owner")
}

func TestRuntimeStepIsRefusedAfterDrain(t *testing.T) {
	e := newStepEngine(t)
	if err := e.Runtime().Drain(context.Background(), stepCall); err != nil {
		t.Fatal(err)
	}
	_, err := e.Runtime().Step(context.Background(), stepCall, uma.WorkerOptions{Owner: "w"})
	requireCode(t, err, model.Unavailable, "draining")
}
