package operations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// losingStore fails the next lose units with model.Transient before they
// run, as a serializable store aborts units lost to concurrent ones.
type losingStore struct {
	store.Store
	mu   sync.Mutex
	lose int
	runs int
}

func (s *losingStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	s.mu.Lock()
	s.runs++
	lost := s.lose > 0
	if lost {
		s.lose--
	}
	s.mu.Unlock()
	if lost {
		return model.NewError(model.Transient, "unit could not be serialized; retry the unit")
	}
	return s.Store.Transact(ctx, t, fn)
}

// pauses records the ceiling of every pause between reruns and pauses for
// none of them.
func pauses(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	t.Cleanup(operations.SetUnitJitter(func(max time.Duration) time.Duration {
		got = append(got, max)
		return 0
	}))
	return &got
}

// A unit lost to concurrent units is rerun after a jittered pause whose
// ceiling doubles per consecutive loss up to its bound, until it commits:
// a caller does not see model.Transient while the reruns last.
func TestTransactRerunsLostUnitsWithGrowingPauses(t *testing.T) {
	r := newRig(t)
	got := pauses(t)
	lose := operations.MaxUnitAttempts - 1
	s := &losingStore{Store: r.Store.Inner(), lose: lose}
	r.Store.Set(s)
	ran := 0
	if err := operations.Transact(r.Ctx, r.E, tenantA, func(store.Tx) error { ran++; return nil }); err != nil {
		t.Fatalf("Transact after %d lost units = %v, want committed", lose, err)
	}
	if s.runs != operations.MaxUnitAttempts || ran != 1 {
		t.Fatalf("units = %d, callback runs = %d; want %d units, 1 run", s.runs, ran, operations.MaxUnitAttempts)
	}
	if len(*got) != lose {
		t.Fatalf("pauses = %v, want one before each of %d reruns", *got, lose)
	}
	want := operations.UnitPauseBase
	for i, c := range *got {
		if c != want {
			t.Fatalf("pause %d ceiling = %v, want %v (all: %v)", i+1, c, want, *got)
		}
		want = min(2*want, operations.UnitPauseMax)
	}
}

// A unit that keeps losing reports model.Transient after the bounded
// number of runs, with no pause after the last.
func TestTransactReportsTransientAfterBoundedReruns(t *testing.T) {
	r := newRig(t)
	got := pauses(t)
	s := &losingStore{Store: r.Store.Inner(), lose: 1 << 20}
	r.Store.Set(s)
	err := operations.Transact(r.Ctx, r.E, tenantA, func(store.Tx) error { return nil })
	requireCode(t, err, model.Transient, "could not be serialized")
	if s.runs != operations.MaxUnitAttempts || len(*got) != operations.MaxUnitAttempts-1 {
		t.Fatalf("units = %d, pauses = %d; want %d units, %d pauses", s.runs, len(*got), operations.MaxUnitAttempts, operations.MaxUnitAttempts-1)
	}
}

// A context ending during a pause ends the reruns with the context's
// error, without waiting the pause out.
func TestTransactPauseHonoursContext(t *testing.T) {
	r := newRig(t)
	t.Cleanup(operations.SetUnitJitter(func(time.Duration) time.Duration { return time.Hour }))
	s := &losingStore{Store: r.Store.Inner(), lose: 1 << 20}
	r.Store.Set(s)
	ctx, cancel := context.WithTimeout(r.Ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := operations.Transact(ctx, r.E, tenantA, func(store.Tx) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("Transact = %v after %v, want the deadline promptly", err, time.Since(start))
	}
	if s.runs != 1 {
		t.Fatalf("units = %d, want 1", s.runs)
	}
}
