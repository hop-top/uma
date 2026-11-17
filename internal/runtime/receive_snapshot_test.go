package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// callCountingStore counts the units of work opened on the store.
type callCountingStore struct {
	store.Store
	n atomic.Int64
}

func (s *callCountingStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	s.n.Add(1)
	return s.Store.View(ctx, t, fn)
}

func (s *callCountingStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	s.n.Add(1)
	return s.Store.Transact(ctx, t, fn)
}

func (r *stateRig) snapshot() (runtime.ReceiverStatus, bool) {
	r.t.Helper()
	return r.h.RT.ReceiverState(r.call, r.h.Channel.Ref)
}

// ReceiverState follows the supervisor through connect, failure and a
// stated wait like ReceiverStatus does, and reads no store.
func TestReceiverStateSnapshotFollowsTheSupervisorWithoutTheStore(t *testing.T) {
	a := newStateAdapter(true,
		stateStep{reports: []bool{true}, err: model.NewError(model.Unavailable, "gateway gone")},
		stateStep{err: statedWaitErr(2 * time.Hour)},
		stateStep{reports: []bool{true}, accept: true},
	)
	var cs *callCountingStore
	r := newStateRig(t, a, statePolicy, func(c *engine.Config) {
		cs = &callCountingStore{Store: c.Store}
		c.Store = cs
	})
	t0 := r.h.Clock.Now()
	r.start("host-a")
	r.held()

	check := func(what string, want runtime.ReceiverStatus) {
		t.Helper()
		got, ok := r.snapshot()
		if !ok {
			t.Fatalf("%s: no snapshot for a running receiver", what)
		}
		if got.Owner != "host-a" || !got.Until.IsZero() {
			t.Fatalf("%s: owner %q until %v, want host-a and no lease expiry", what, got.Owner, got.Until)
		}
		wantState(t, what, got, want)
	}
	check("connected", runtime.ReceiverStatus{Phase: runtime.ReceiverRunning, Since: t0, Link: runtime.ReceiverConnected})

	a.release <- struct{}{}
	r.waiting()
	check("after a failure", runtime.ReceiverStatus{
		Phase: runtime.ReceiverRetrying, Since: t0, Failures: 1, Reason: model.Unavailable, RetryAt: t0.Add(time.Second),
	})

	t1 := r.h.Clock.Advance(5 * time.Second)
	r.w.go_ <- struct{}{}
	r.held()
	check("reconnecting", runtime.ReceiverStatus{
		Phase: runtime.ReceiverRunning, Since: t1, Failures: 1, Reason: model.Unavailable, Link: runtime.ReceiverConnecting,
	})
	a.release <- struct{}{}
	r.waiting()
	check("stated wait", runtime.ReceiverStatus{
		Phase: runtime.ReceiverRetrying, Since: t1, Failures: 2, Reason: model.RateLimited, RetryAt: t1.Add(2 * time.Hour),
	})
	// The supervisor is parked in its wait, so the store sees only us.
	before := cs.n.Load()
	for range 100 {
		r.snapshot()
	}
	if n := cs.n.Load() - before; n != 0 {
		t.Fatalf("100 ReceiverState calls opened %d store units, want 0", n)
	}
	r.h.RT.ReceiverStatus(r.h.Ctx, r.call, r.h.Channel.Ref) //nolint:errcheck // control: the counter sees store reads
	if cs.n.Load() == before {
		t.Fatal("counting store saw no ReceiverStatus read")
	}
}

// No receiver here, no snapshot: an unknown channel, another tenant's
// channel, an invalid principal, a call that returned, and a runtime that
// does not run the call.
func TestReceiverStateSnapshotIsFalseWithoutALocalReceiver(t *testing.T) {
	a := newStateAdapter(true, stateStep{reports: []bool{true}})
	r := newStateRig(t, a, statePolicy)
	if _, ok := r.snapshot(); ok {
		t.Fatal("snapshot before Receive started")
	}
	r.start("host-a")
	r.held()
	if _, ok := r.snapshot(); !ok {
		t.Fatal("no snapshot while running")
	}
	other := r.h.Channel.Ref
	other.ID = "nope"
	if _, ok := r.h.RT.ReceiverState(r.call, other); ok {
		t.Fatal("snapshot of a channel not running here")
	}
	foreign := r.h.Channel.Ref
	foreign.Tenant = "elsewhere"
	if _, ok := r.h.RT.ReceiverState(r.call, foreign); ok {
		t.Fatal("snapshot across tenants")
	}
	if _, ok := r.h.RT.ReceiverState(runtime.Call{}, r.h.Channel.Ref); ok {
		t.Fatal("snapshot for an invalid principal")
	}
	if _, ok := runtime.New(r.h.E).ReceiverState(r.call, r.h.Channel.Ref); ok {
		t.Fatal("snapshot from a runtime that does not run the call")
	}
	r.cancel()
	_ = r.result()
	if _, ok := r.snapshot(); ok {
		t.Fatal("snapshot after Receive returned")
	}
}

// ReceiverState is read from other goroutines while the supervisor and the
// adapter write: run with -race.
func TestReceiverStateSnapshotConcurrentReads(t *testing.T) {
	var fails []stateStep
	for range 6 {
		fails = append(fails, stateStep{reports: []bool{true, false}, err: model.NewError(model.Transient, "x")})
	}
	a := newStateAdapter(true, fails...)
	r := newStateRig(t, a, engine.RetryPolicy{MaxAttempts: 20, BaseDelay: time.Second, MaxDelay: 8 * time.Second})
	r.start("host-a")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if st, ok := r.snapshot(); ok {
					switch st.Phase {
					case runtime.ReceiverRunning, runtime.ReceiverRetrying, runtime.ReceiverEnded:
					default:
						t.Errorf("phase = %q", st.Phase)
					}
					reads.Add(1)
				}
			}
		})
	}
	for range fails {
		n := r.held()
		report := a.reportFn(n)
		for range 3 {
			wg.Go(func() {
				for j := range 200 {
					report(j%2 == 0)
				}
			})
		}
		a.release <- struct{}{}
		r.waiting()
		r.w.go_ <- struct{}{}
	}
	r.held()
	// Still running and held: a snapshot is readable until cancel. Readers
	// may not have been scheduled yet on a slow runner, so wait for one.
	deadline := time.Now().Add(5 * time.Second)
	for reads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.cancel()
	_ = r.result()
	close(stop)
	wg.Wait()
	if reads.Load() == 0 {
		t.Fatal("no snapshot was read")
	}
}
