package runtime_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Units lost to concurrent units after a claim. A serializable store aborts
// one of two conflicting units with model.Transient; under contention one
// unit can lose every rerun the engine gives it. The rig's clock never
// moves, so an item its worker abandons under a live lease is never
// reclaimed: each case fails by stranding the item, the way a loaded
// fleet stalls until the lease expires.

// lossyStore loses the next lose units matching match after their callback
// succeeded, as a store aborting them at commit would. A lost unit rolls
// back.
type lossyStore struct {
	store.Store
	mu    sync.Mutex
	match func(calls []string) bool
	lose  int
	lost  int
}

func (s *lossyStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		spy := &spyTx{Tx: tx}
		if err := fn(spy); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lose > 0 && s.match(spy.calls) {
			s.lose--
			s.lost++
			return model.NewError(model.Transient, "unit could not be serialized; retry the unit")
		}
		return nil
	})
}

func (s *lossyStore) lostUnits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}

// spyTx records the store calls of one unit.
type spyTx struct {
	store.Tx
	calls []string
}

func (t *spyTx) ClaimWork(ctx context.Context, c store.WorkClaim) ([]store.Work, error) {
	t.calls = append(t.calls, "ClaimWork")
	return t.Tx.ClaimWork(ctx, c)
}

func (t *spyTx) UpdateOperation(ctx context.Context, op model.Operation, expected model.Revision, f store.Fence) error {
	t.calls = append(t.calls, "UpdateOperation")
	return t.Tx.UpdateOperation(ctx, op, expected, f)
}

func (t *spyTx) AppendAttemptEvidence(ctx context.Context, ev store.AttemptEvidence) error {
	t.calls = append(t.calls, "AppendAttemptEvidence")
	return t.Tx.AppendAttemptEvidence(ctx, ev)
}

func (t *spyTx) CompleteWork(ctx context.Context, w store.Work) error {
	t.calls = append(t.calls, "CompleteWork")
	return t.Tx.CompleteWork(ctx, w)
}

func calling(names ...string) func([]string) bool {
	return func(calls []string) bool {
		for _, n := range names {
			if !slices.Contains(calls, n) {
				return false
			}
		}
		return true
	}
}

// unitRetries is how often the engine runs one unit before it reports
// model.Transient: losing this many in a row exhausts a unit.
const unitRetries = 3

func loseUnits(r *rig, lose int, match func([]string) bool) *lossyStore {
	s := &lossyStore{Store: r.Store.Inner(), match: match, lose: lose}
	r.Store.Set(s)
	return s
}

func workDepth(t *testing.T, r *rig) store.WorkDepth {
	t.Helper()
	var d store.WorkDepth
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		var err error
		d, err = rd.WorkDepth(r.Ctx, operations.WorkKindOperation)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The unit recording the attempt before the provider call loses every
// rerun: the worker still holds the lease, so it sends and settles the item
// itself instead of leaving it for the lease to expire.
func TestLostAttemptUnitDoesNotStrandItem(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	lossy := loseUnits(r, unitRetries, calling("UpdateOperation", "AppendAttemptEvidence"))
	r.Step("w")
	if lossy.lostUnits() != unitRetries {
		t.Fatalf("lost %d attempt units, want %d", lossy.lostUnits(), unitRetries)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if d := workDepth(t, r); d != (store.WorkDepth{}) {
		t.Fatalf("work depth = %+v, want the item completed", d)
	}
}

// The unit settling the operation after the provider answered loses every
// rerun: the effect is not sent again and the item is settled by its
// owner.
func TestLostSettleUnitDoesNotStrandItem(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	lossy := loseUnits(r, unitRetries, calling("UpdateOperation", "CompleteWork"))
	r.Step("w")
	if lossy.lostUnits() != unitRetries {
		t.Fatalf("lost %d settle units, want %d", lossy.lostUnits(), unitRetries)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if d := workDepth(t, r); d != (store.WorkDepth{}) {
		t.Fatalf("work depth = %+v, want the item completed", d)
	}
}

// A claim round that lost every rerun to concurrent claims says nothing
// about the queue: Run claims again at once instead of waiting a poll
// interval with work queued.
func TestLostClaimRoundDoesNotIdle(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	lossy := loseUnits(r, unitRetries, calling("ClaimWork"))
	done := startRun(r, r.Ctx, "w")
	<-waiterOf(r).Idle
	if lossy.lostUnits() != unitRetries {
		t.Fatalf("lost %d claim units, want %d", lossy.lostUnits(), unitRetries)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends at the first idle wait = %d, want 1", n)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
