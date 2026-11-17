package operations_test

import (
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

const sweepWindow = 10 * time.Minute

func sweep(t *testing.T, r *enginetest.Rig) operations.ExpireReport {
	t.Helper()
	rep, err := operations.ExpireHeld(r.Ctx, r.E, tenantA, sweepWindow)
	if err != nil {
		t.Fatalf("ExpireHeld: %v", err)
	}
	return rep
}

func TestExpireHeldPastDeadlineAppliesNothing(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	if rep := sweep(t, r); rep.Expired != 0 {
		t.Fatalf("before the deadline: %+v", rep)
	}
	requireHeld(t, r, held.Ref)

	r.Clock.Advance(time.Hour)
	if rep := sweep(t, r); rep.Expired != 1 {
		t.Fatalf("report = %+v, want 1 expired", rep)
	}
	got := r.Op(held.Ref)
	if got.State != model.Expired || got.Held.Decision != model.HeldExpired || got.Held.DecidedAt == nil ||
		!got.Held.DecidedAt.Equal(r.Clock.Now()) || got.Held.DecidedBy != "" || got.Revision == held.Revision {
		t.Fatalf("expired = %s %+v", got.State, got.Held)
	}
	// Nothing queued, nothing sent.
	if n, err := r.RT.Step(r.Ctx, runtime.Call{Principal: enginetest.PrincipalA()}, runtime.WorkerOptions{Owner: "w"}); err != nil || n != 0 {
		t.Fatalf("Step = %d, %v; want no work", n, err)
	}
	if sent := sentTexts(r); len(sent) != 0 {
		t.Fatalf("sent %v", sent)
	}
}

func TestExpireHeldIsIdempotent(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(time.Hour)
	sweep(t, r)
	first := r.Op(held.Ref)
	r.Clock.Advance(time.Hour)
	if rep := sweep(t, r); rep != (operations.ExpireReport{}) {
		t.Fatalf("second sweep = %+v, want nothing", rep)
	}
	if again := r.Op(held.Ref); again.Revision != first.Revision || !again.Held.DecidedAt.Equal(*first.Held.DecidedAt) {
		t.Fatalf("second sweep rewrote the operation: %+v vs %+v", again, first)
	}
}

func TestExpiredOperationRefusesDecisions(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(time.Hour)
	sweep(t, r)
	exp := r.Op(held.Ref)
	_, err := r.C.Operations().Approve(r.Ctx, decide(exp))
	requireCode(t, err, model.Conflict, "")
	_, err = r.C.Operations().Reject(r.Ctx, decide(exp))
	requireCode(t, err, model.Conflict, "")
	if got := r.Op(held.Ref); got.State != model.Expired {
		t.Fatalf("state = %s", got.State)
	}
}

func TestExpireHeldFreesDuplicateGuard(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	propose(t, r, c, replyProposal(r, "p-1", "one"))
	r.Clock.Advance(time.Hour)
	sweep(t, r)
	second := replyProposal(r, "p-2", "two")
	if a := propose(t, r, c, second); a.AlreadyHeld || a.Operation.State != model.Held {
		t.Fatalf("second proposal = %+v, want a fresh held operation", a)
	}
}

func TestExpireHeldWithoutDeadlineStaysHeld(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	s := replyProposal(r, "p-1", "hello")
	s.Hold.ExpiresAt = nil
	held := propose(t, r, c, s).Operation
	r.Clock.Advance(1000 * time.Hour)
	if rep := sweep(t, r); rep.Expired != 0 {
		t.Fatalf("report = %+v", rep)
	}
	requireHeld(t, r, held.Ref)
}

func TestExpireHeldConcurrentSweepsOneTransition(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(time.Hour)
	const sweepers = 8
	reps := make([]operations.ExpireReport, sweepers)
	errs := make([]error, sweepers)
	var wg sync.WaitGroup
	for i := range sweepers {
		wg.Go(func() { reps[i], errs[i] = operations.ExpireHeld(r.Ctx, r.E, tenantA, sweepWindow) })
	}
	wg.Wait()
	total := 0
	for i := range reps {
		if errs[i] != nil {
			t.Fatalf("sweeper %d: %v", i, errs[i])
		}
		total += reps[i].Expired
	}
	if total != 1 {
		t.Fatalf("transitions = %d (%+v), want exactly one", total, reps)
	}
	if got := r.Op(held.Ref); got.State != model.Expired {
		t.Fatalf("state = %s", got.State)
	}
}

func TestCarryOverMarksHeldAfterOneWindow(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	s := replyProposal(r, "p-1", "hello")
	exp := r.Clock.Now().Add(24 * time.Hour).UTC()
	s.Hold.ExpiresAt = &exp
	held := propose(t, r, c, s).Operation

	r.Clock.Advance(sweepWindow - time.Second)
	if rep := sweep(t, r); rep.CarriedOver != 0 || r.Op(held.Ref).Held.CarriedOverAt != nil {
		t.Fatalf("inside the first window: %+v", rep)
	}
	r.Clock.Advance(time.Second)
	if rep := sweep(t, r); rep.CarriedOver != 1 {
		t.Fatalf("report = %+v, want 1 carried over", rep)
	}
	got := r.Op(held.Ref)
	if got.State != model.Held || got.Held.CarriedOverAt == nil || !got.Held.CarriedOverAt.Equal(r.Clock.Now()) {
		t.Fatalf("carried = %s %+v", got.State, got.Held)
	}
	// Set once; the duplicate guard still finds it.
	r.Clock.Advance(sweepWindow)
	if rep := sweep(t, r); rep != (operations.ExpireReport{}) {
		t.Fatalf("second sweep = %+v", rep)
	}
	if again := r.Op(held.Ref); again.Revision != got.Revision {
		t.Fatalf("marker rewritten")
	}
	if a := propose(t, r, c, replyProposal(r, "p-2", "other")); !a.AlreadyHeld || a.Operation.Ref != held.Ref {
		t.Fatalf("duplicate guard = %+v", a)
	}
	// Still decidable after carry-over, from the fresh revision.
	if _, err := r.C.Operations().Approve(r.Ctx, decide(got)); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

func TestCarryOverNeverMarksExpired(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(time.Hour) // past both the window and the deadline
	rep := sweep(t, r)
	if rep.Expired != 1 || rep.CarriedOver != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if got := r.Op(held.Ref); got.Held.CarriedOverAt != nil {
		t.Fatalf("expired operation carries over: %+v", got.Held)
	}
}

// A sweep working from a scan taken before another sweep (or a decision)
// moved the operation leaves it alone: it is no longer held.
func TestExpireHeldStaleScanDoesNotMoveTwice(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(time.Hour)
	stale, err := operations.HeldRefs(r.Ctx, r.E, tenantA)
	if err != nil || len(stale) != 1 {
		t.Fatalf("scan = %v, %v", stale, err)
	}
	sweep(t, r)
	first := r.Op(held.Ref)
	rep, err := operations.ExpireRefs(r.Ctx, r.E, tenantA, stale, sweepWindow)
	if err != nil || rep != (operations.ExpireReport{}) {
		t.Fatalf("stale sweep = %+v, %v", rep, err)
	}
	if again := r.Op(held.Ref); again.Revision != first.Revision {
		t.Fatalf("stale sweep rewrote the expired operation")
	}
}

func expiredEvents(r *enginetest.Rig) []model.Event {
	return heldEvents(r, model.EventOperationExpired)
}

func TestExpireHeldEmitsOneEventPerTransition(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	sweep(t, r)
	if n := len(expiredEvents(r)); n != 0 {
		t.Fatalf("events before the deadline = %d", n)
	}
	r.Clock.Advance(time.Hour)
	sweep(t, r)
	sweep(t, r)
	evs := expiredEvents(r)
	if len(evs) != 1 {
		t.Fatalf("operation.expired events = %d, want 1", len(evs))
	}
	if p := heldPayload(t, evs[0]); p.Operation != held.Ref || p.Decision != model.HeldExpired ||
		evs[0].Revision != r.Op(held.Ref).Revision {
		t.Fatalf("event = %+v payload = %+v", evs[0], p)
	}
}

func TestExpireHeldConcurrentLosersEmitNoEvent(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	propose(t, r, c, replyProposal(r, "p-1", "hello"))
	r.Clock.Advance(time.Hour)
	stale, err := operations.HeldRefs(r.Ctx, r.E, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	sweep(t, r)
	if _, err := operations.ExpireRefs(r.Ctx, r.E, tenantA, stale, sweepWindow); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() { _, _ = operations.ExpireHeld(r.Ctx, r.E, tenantA, sweepWindow) })
	}
	wg.Wait()
	if n := len(expiredEvents(r)); n != 1 {
		t.Fatalf("operation.expired events = %d, want exactly 1", n)
	}
}
