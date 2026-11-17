package conformance

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// overlapMeetWait bounds how long a unit waits inside its callback for the
// others to enter theirs. A store whose units overlap meets in
// milliseconds; one that serializes callbacks fails the case after this.
const overlapMeetWait = 3 * time.Second

// overlapGate holds each unit inside its callback until all have entered.
type overlapGate struct {
	t       *testing.T
	failed  bool
	n       int
	mu      sync.Mutex
	arrived int
	all     chan struct{}
	once    sync.Once
}

func newOverlapGate(t *testing.T, n int) *overlapGate {
	return &overlapGate{t: t, n: n, all: make(chan struct{})}
}

// meet blocks the calling unit until every unit is inside its callback, and
// reports the failure once if they never are.
func (g *overlapGate) meet() {
	g.mu.Lock()
	g.arrived++
	if g.arrived == g.n {
		close(g.all)
	}
	g.mu.Unlock()
	timer := time.NewTimer(overlapMeetWait)
	defer timer.Stop()
	select {
	case <-g.all:
	case <-timer.C:
		g.once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.failed = true
			g.t.Errorf("units did not overlap: a unit stayed inside its callback for %v while %d of %d other units never entered theirs. "+
				"A store must not hold a store-wide or tenant-wide lock from the start of a callback to its commit "+
				"(see \"Units of work\" in the store package documentation)", overlapMeetWait, g.n-g.arrived, g.n-1)
		})
	}
}

// overlapUnits runs one unit per fn at once, each in its tenant, and returns
// each unit's outcome. A unit calls meet to wait for the others. A unit that
// fails is not retried.
func (e *env) overlapUnits(t *testing.T, tenants []model.TenantID, fns ...func(tx store.Tx, meet func()) error) []error {
	t.Helper()
	gate := newOverlapGate(t, len(fns))
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Go(func() {
			errs[i] = e.s.Transact(e.ctx, tenants[i], func(tx store.Tx) error { return fn(tx, gate.meet) })
		})
	}
	wg.Wait()
	if gate.failed {
		// The units never overlapped, so their outcomes say nothing more.
		t.FailNow()
	}
	return errs
}

func (e *env) channelAt(t *testing.T, ref model.ChannelRef) (model.Channel, error) {
	t.Helper()
	var ch model.Channel
	err := e.s.View(e.ctx, ref.Tenant, func(r store.Reader) error {
		var err error
		ch, err = r.GetChannel(e.ctx, ref)
		return err
	})
	return ch, err
}

// wantLoser requires a unit that lost an overlap to report the store's
// conflict or serialization failure.
func wantLoser(t *testing.T, what string, err error) {
	t.Helper()
	if !model.IsCode(err, model.Conflict) && !model.IsCode(err, model.Transient) {
		t.Fatalf("%s: error = %v, want model.Conflict or model.Transient", what, err)
	}
}

// unitOverlapCases pin that units of work overlap and that a conflict
// between overlapping units surfaces at the conflicting write or at commit:
// one unit commits, the other reports model.Conflict or model.Transient and
// stores nothing, and no unit's committed write is silently replaced.
var unitOverlapCases = []storeCase{
	{"UnitsOverlapInOneTenant", func(t *testing.T, e *env) {
		refs := []model.ChannelRef{{Tenant: tenantA, ID: e.id(t)}, {Tenant: tenantA, ID: e.id(t)}}
		unit := func(i int) func(store.Tx, func()) error {
			return func(tx store.Tx, meet func()) error {
				if _, err := tx.GetChannel(e.ctx, e.channel); err != nil {
					return err
				}
				meet()
				return tx.PutChannel(e.ctx, e.channelRecord(refs[i], "r1"), "")
			}
		}
		errs := e.overlapUnits(t, []model.TenantID{tenantA, tenantA}, unit(0), unit(1))
		// Writes to different records may both commit, or the store may fail
		// one with model.Transient (a tenant-wide version check); the loser
		// stores nothing and the engine runs it again.
		for i, err := range errs {
			_, readErr := e.channelAt(t, refs[i])
			switch {
			case err == nil:
				if readErr != nil {
					t.Errorf("unit %d committed but its channel reads %v", i, readErr)
				}
			case model.IsCode(err, model.Transient):
				wantCode(t, readErr, model.NotFound, "channel of unit ", i, " that failed")
				if err := e.retryTx(func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(refs[i], "r1"), "") }); err != nil {
					t.Errorf("unit %d rerun: %v", i, err)
				}
			default:
				t.Errorf("unit %d: %v, want success or model.Transient for a write no other unit touched", i, err)
			}
		}
		for i, ref := range refs {
			if _, err := e.channelAt(t, ref); err != nil {
				t.Errorf("channel of unit %d: %v, want it stored", i, err)
			}
		}
	}},
	{"UnitsOverlapAcrossTenants", func(t *testing.T, e *env) {
		other := model.ChannelRef{Tenant: tenantB, ID: e.id(t)}
		mine := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		put := func(ref model.ChannelRef) func(store.Tx, func()) error {
			return func(tx store.Tx, meet func()) error {
				meet()
				return tx.PutChannel(e.ctx, e.channelRecord(ref, "r1"), "")
			}
		}
		errs := e.overlapUnits(t, []model.TenantID{tenantA, tenantB}, put(mine), put(other))
		for i, err := range errs {
			if err != nil && !model.IsCode(err, model.Transient) {
				t.Errorf("unit %d: %v, want success or model.Transient", i, err)
			}
		}
		for i, ref := range []model.ChannelRef{mine, other} {
			if _, err := e.channelAt(t, ref); err != nil && errs[i] == nil {
				t.Errorf("channel of %s: %v, want it stored", ref.Tenant, err)
			}
		}
	}},
	{"ViewRunsWhileATransactIsOpen", func(t *testing.T, e *env) {
		held := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		inside, release := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		var txErr error
		wg.Go(func() {
			txErr = e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
				if err := tx.PutChannel(e.ctx, e.channelRecord(held, "r1"), ""); err != nil {
					close(inside)
					return err
				}
				close(inside)
				<-release
				return nil
			})
		})
		<-inside
		viewed := make(chan error, 1)
		go func() {
			viewed <- e.s.View(e.ctx, tenantA, func(r store.Reader) error {
				_, err := r.GetChannel(e.ctx, held)
				if !model.IsCode(err, model.NotFound) {
					return fmt.Errorf("read of an uncommitted write = %v, want model.NotFound", err)
				}
				return nil
			})
		}()
		select {
		case err := <-viewed:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(overlapMeetWait):
			t.Errorf("a View waited %v for an open Transact in its tenant: a callback never waits for another unit's callback to finish", overlapMeetWait)
		}
		close(release)
		wg.Wait()
		if txErr != nil {
			t.Fatalf("Transact: %v", txErr)
		}
		if _, err := e.channelAt(t, held); err != nil {
			t.Errorf("committed channel: %v", err)
		}
	}},
	{"OverlappingUpdatesOfOneRecordCommitOnce", func(t *testing.T, e *env) {
		// Both units read the channel at r1, then both write r2 over it.
		extra := []model.ChannelRef{{Tenant: tenantA, ID: e.id(t)}, {Tenant: tenantA, ID: e.id(t)}}
		revs := []model.Revision{"r2-first", "r2-second"}
		unit := func(i int) func(store.Tx, func()) error {
			return func(tx store.Tx, meet func()) error {
				if got, err := tx.GetChannel(e.ctx, e.channel); err != nil || got.Revision != "r1" {
					return fmt.Errorf("read channel = %q, %v, want r1", got.Revision, err)
				}
				meet()
				if err := tx.PutChannel(e.ctx, e.channelRecord(e.channel, revs[i]), "r1"); err != nil {
					return err
				}
				return tx.PutChannel(e.ctx, e.channelRecord(extra[i], "r1"), "")
			}
		}
		errs := e.overlapUnits(t, []model.TenantID{tenantA, tenantA}, unit(0), unit(1))
		winner := raceWinner(t, "update of one record", errs)
		got, err := e.channelAt(t, e.channel)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != revs[winner] {
			t.Errorf("channel revision = %q, want the winner's %q", got.Revision, revs[winner])
		}
		for i, ref := range extra {
			_, err := e.channelAt(t, ref)
			switch {
			case i == winner && err != nil:
				t.Errorf("winner's second write: %v, want it committed with the first", err)
			case i != winner && !model.IsCode(err, model.NotFound):
				t.Errorf("loser's second write: %v, want it rolled back with the first", err)
			}
		}
	}},
	{"OverlappingCreatesOfOneRecordCommitOnce", func(t *testing.T, e *env) {
		// Both units find the record absent, then both create it.
		ref := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		revs := []model.Revision{"c-first", "c-second"}
		unit := func(i int) func(store.Tx, func()) error {
			return func(tx store.Tx, meet func()) error {
				if _, err := tx.GetChannel(e.ctx, ref); !model.IsCode(err, model.NotFound) {
					return fmt.Errorf("read of an absent channel = %v, want model.NotFound", err)
				}
				meet()
				return tx.PutChannel(e.ctx, e.channelRecord(ref, revs[i]), "")
			}
		}
		errs := e.overlapUnits(t, []model.TenantID{tenantA, tenantA}, unit(0), unit(1))
		winner := raceWinner(t, "create of one record", errs)
		got, err := e.channelAt(t, ref)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != revs[winner] {
			t.Errorf("channel revision = %q, want the winner's %q", got.Revision, revs[winner])
		}
	}},
}

// raceWinner requires exactly one of two overlapping units to commit and the
// other to lose with a conflict, and returns the winner.
func raceWinner(t *testing.T, what string, errs []error) int {
	t.Helper()
	winner := -1
	for i, err := range errs {
		if err != nil {
			continue
		}
		if winner >= 0 {
			t.Fatalf("overlapping %s: units %d and %d both committed; the second write silently replaced the first (last writer wins). "+
				"The loser must fail with model.Conflict or model.Transient at its write or at commit", what, winner, i)
		}
		winner = i
	}
	if winner < 0 {
		t.Fatalf("overlapping %s: no unit committed: %v", what, errs)
	}
	wantLoser(t, what, errs[1-winner])
	return winner
}
