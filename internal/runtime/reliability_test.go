package runtime_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

type observations struct {
	mu  sync.Mutex
	obs []ports.Observation
}

func (o *observations) Observe(_ context.Context, ob ports.Observation) {
	o.mu.Lock()
	o.obs = append(o.obs, ob)
	o.mu.Unlock()
}

func (o *observations) kinds(kind string) []ports.Observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []ports.Observation
	for _, ob := range o.obs {
		if ob.Kind == kind {
			out = append(out, ob)
		}
	}
	return out
}

func withWork(p *engine.WorkPolicy) enginetest.RigOption {
	return func(c *engine.Config, _ *enginetest.OutFactory) { c.Work = p }
}

func withObserver(o ports.Observer) enginetest.RigOption {
	return func(c *engine.Config, _ *enginetest.OutFactory) { c.Observer = o }
}

// abandon claims every claimable item as owner and lets the claim expire
// without running it, as a crashed worker would.
func abandon(r *rig, owner string) int {
	r.T.Helper()
	n := len(r.Claim(owner))
	r.Clock.Advance(2 * time.Minute)
	return n
}

func deliveryStates(t *testing.T, r *rig) []model.DeliveryState {
	t.Helper()
	var out []model.DeliveryState
	for _, ev := range r.Events() {
		if ev.Kind != model.EventDeliveryUpdated {
			continue
		}
		var p struct {
			State model.DeliveryState `json:"state"`
		}
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.State)
	}
	return out
}

// Work whose claims keep ending unfinished is quarantined on the claim past
// the budget: nothing reaches the provider, the operation is dead_lettered
// with a safe failure, a delivery event records it, and no later round
// claims it again.
func TestPoisonWorkIsQuarantined(t *testing.T) {
	r := newRig(t, withWork(&engine.WorkPolicy{MaxClaims: 2, MaxInFlight: 10}))
	a := r.Reply("op-1", "hello")
	if abandon(r, "crashed-1") != 1 || abandon(r, "crashed-2") != 1 {
		t.Fatal("work not claimable before quarantine")
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("third claim ran %d items, want the poisoned one", n)
	}
	if sends := r.F.ProviderSends(); len(sends) != 0 {
		t.Fatalf("provider sends = %d, want none for poisoned work", len(sends))
	}
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.DeadLettered, model.Permanent)
	if op.Failure.Retry != model.NoRetry {
		t.Errorf("failure retry = %q, want none", op.Failure.Retry)
	}
	if got := deliveryStates(t, r); !slices.Equal(got, []model.DeliveryState{model.DeadLettered}) {
		t.Errorf("delivery events = %v, want one dead_lettered", got)
	}
	r.Clock.Advance(time.Hour)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("quarantined work claimed again: %d items", n)
	}
}

// An operation whose last attempt is unresolved stays outcome_unknown in
// its dead-letter failure: a replay reconciles before any new attempt.
func TestPoisonWithUnresolvedAttemptKeepsOutcomeUnknown(t *testing.T) {
	r := newRig(t, withWork(&engine.WorkPolicy{MaxClaims: 1, MaxInFlight: 10}), withReconciler)
	entered := make(chan struct{})
	r.F.Respond(func(ctx context.Context, _ adapter.SendRequest) (adapter.Outcome, error, bool) {
		close(entered)
		<-ctx.Done()
		return adapter.Outcome{}, ctx.Err(), false
	})
	a := r.Reply("op-1", "hello")
	ctx, cancel := context.WithCancel(r.Ctx)
	works := r.Claim("crashed")
	done := make(chan error, 1)
	go func() { done <- r.RT.ExecuteClaimed(ctx, tenantA, works[0]) }()
	<-entered
	cancel()
	<-done
	r.Clock.Advance(2 * time.Minute)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.DeadLettered, model.OutcomeUnknown)
}

// Replay needs current authorization, moves a dead-lettered operation back
// to queued with its work requeued, replays for the same operation ID and
// conflicts for an operation that is not dead-lettered.
func TestReplayRequeuesDeadLetteredWork(t *testing.T) {
	r := newRig(t, withWork(&engine.WorkPolicy{MaxClaims: 1, MaxInFlight: 10}))
	a := r.Reply("op-1", "hello")
	abandon(r, "crashed")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.DeadLettered, model.Permanent)

	ops := r.C.Operations()
	req := model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "admin"}, Ref: a.Operation.Ref}
	cmd := model.Command{Call: model.Call{Correlation: "admin"}, Operation: "replay-1"}
	r.Policy.Set(true)
	_, err := ops.Replay(r.Ctx, req, cmd)
	requireCode(t, err, model.Unauthorized, "")
	if got := r.Policy.Actions(); got[len(got)-1] != model.ActionOperationReplay {
		t.Fatalf("last policy check = %q, want %q", got[len(got)-1], model.ActionOperationReplay)
	}
	r.Policy.Set(false)
	requireState(t, r.Op(a.Operation.Ref), model.DeadLettered, model.Permanent)

	acc, err := ops.Replay(r.Ctx, req, cmd)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if acc.Replayed || acc.Operation.State != model.Committed {
		t.Fatalf("replay acceptance = %+v", acc)
	}
	if k := acc.Operation.Key; k == nil || k.Kind != model.ActionOperationReplay || k.Target != a.Operation.Ref.Ref() || k.ID != "replay-1" {
		t.Fatalf("replay acceptance key = %+v, want operation.replay on the replayed operation", k)
	}
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Queued, "")
	if op.Failure != nil {
		t.Errorf("failure after replay = %+v, want cleared", op.Failure)
	}
	again, err := ops.Replay(r.Ctx, req, cmd)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("repeated replay = %+v, %v; want the stored one", again, err)
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("after replay %d items ran, want the requeued one", n)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := deliveryStates(t, r); !slices.Equal(got, []model.DeliveryState{model.DeadLettered, model.Queued, model.ProviderAccepted}) {
		t.Errorf("delivery events = %v", got)
	}
	_, err = ops.Replay(r.Ctx, req, model.Command{Call: model.Call{Correlation: "admin"}, Operation: "replay-2"})
	requireCode(t, err, model.Conflict, "not dead-lettered")
}

// A claim round the in-flight cap leaves empty fails with unavailable and
// is observed as queue_saturated; every round reports its backlog depth.
func TestSaturatedClaimRoundIsSignalled(t *testing.T) {
	obs := &observations{}
	r := newRig(t, withWork(&engine.WorkPolicy{MaxClaims: 10, MaxInFlight: 1}), withObserver(obs))
	r.Reply("op-1", "one")
	r.Reply("op-2", "two")
	if got := r.Claim("a"); len(got) != 1 {
		t.Fatalf("first claim = %d items, want 1 under a cap of 1", len(got))
	}
	_, err := r.RT.ClaimWork(r.Ctx, tenantA, runtime.WorkerOptions{
		Owner: "b", Limit: 10, Kinds: []string{"operation.execute"}, Lease: time.Minute,
	})
	requireCode(t, err, model.Unavailable, "in-flight")
	if n := len(obs.kinds(ports.ObservationQueueSaturated)); n != 1 {
		t.Fatalf("queue_saturated observations = %d, want 1", n)
	}
	backlog := obs.kinds(ports.ObservationBacklog)
	if len(backlog) != 2 || backlog[0].Value != 1 || backlog[1].Value != 1 {
		t.Fatalf("backlog = %+v, want depth 1 after each round", backlog)
	}
}

// Run waits out a saturated round like an idle one instead of failing.
func TestRunWaitsOutSaturation(t *testing.T) {
	obs := &observations{}
	r := newRig(t, withWork(&engine.WorkPolicy{MaxClaims: 10, MaxInFlight: 1}), withObserver(obs))
	r.Reply("op-1", "one")
	r.Reply("op-2", "two")
	r.Claim("other")
	done := startRun(r, r.Ctx, "w")
	select {
	case <-waiterOf(r).Idle:
	case err := <-done:
		t.Fatalf("Run returned %v at saturation, want it waiting", err)
	}
	if len(obs.kinds(ports.ObservationQueueSaturated)) == 0 {
		t.Fatal("saturated round not observed")
	}
	if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run after drain: %v", err)
	}
}
