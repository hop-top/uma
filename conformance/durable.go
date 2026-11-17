package conformance

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// RestartFunc simulates a crash and restart of the process holding fx.Store.
// It abandons fx.Store without closing it gracefully and without waiting for
// units still running on it, then returns a fixture whose Store is a new
// handle opened on the same persisted state. The returned clock continues
// from fx's: leases committed before the restart are judged against it. It
// may register cleanup with t.Cleanup, for example to close the abandoned
// handle once the case ends.
type RestartFunc func(t *testing.T, fx StoreFixture) StoreFixture

// RunDurableSuite runs the durable store cases against factory: every
// RunDurableLogicalSuite case, then crash and restart cases that use restart
// to reopen persisted state. Committed units must survive a restart with
// their claims, ledger, leases, events and checkpoints intact; a unit still
// running when the process stopped must leave no trace. The fault cases run
// again with the engine's process crashing on one store handle and the next
// process recovering on the restarted one.
//
// Passing RunDurableSuite is the conformance evidence behind
// Properties().Durable. The suite fails when restart is nil or the store
// reports Durable=false: a store that cannot restart runs
// RunDurableLogicalSuite instead, and makes no durability claim.
func RunDurableSuite(t *testing.T, factory StoreFactory, restart RestartFunc) {
	t.Helper()
	if restart == nil {
		t.Fatal(errNoRestart)
	}
	runDurableGroups(t, factory, restart, append(logicalDurableGroups(), durableGroup{"Restart", restartCases}))
}

// RunDurableLogicalSuite runs the RunDurableSuite cases that need no restart:
// concurrent claims across goroutines (identity, conversation, ingress,
// operation, work and lease), commit versus publication ordering, and replay
// of committed claims, the operation ledger, lease generations and
// checkpoints across later units. Its fault cases run the engine over the
// store and crash its process around the attempt record, the provider
// call, the outcome commit, ingress and checkpoint commits, and while a
// worker or receiver holds a lease; the next process, on the same store,
// must recover without a duplicate effect, a duplicate message or a lost
// batch.
//
// Any store, durable or not, can run it. Passing it is logical evidence only,
// never durability evidence: the memory store passes it and still reports
// Durable=false.
func RunDurableLogicalSuite(t *testing.T, factory StoreFactory) {
	t.Helper()
	runDurableGroups(t, factory, nil, logicalDurableGroups())
}

var (
	errNoRestart  = errors.New("RunDurableSuite needs a RestartFunc; a store that cannot restart runs RunDurableLogicalSuite")
	errNotDurable = errors.New("RunDurableSuite needs a store reporting Properties().Durable=true; a non-durable store runs RunDurableLogicalSuite")
)

// durableSuiteError reports why RunDurableSuite cannot accept a store with
// properties p and restart function restart.
func durableSuiteError(p store.Properties, restart RestartFunc) error {
	switch {
	case restart == nil:
		return errNoRestart
	case !p.Durable:
		return errNotDurable
	}
	return nil
}

// durableCase is one durable scenario. cross is called at the point where
// state must persist: a no-op in the logical suite, a restart in the
// Restart group.
type durableCase struct {
	name string
	run  func(t *testing.T, e *env, cross func(*testing.T))
}

type durableGroup struct {
	name  string
	cases []durableCase
}

func logicalDurableGroups() []durableGroup {
	return []durableGroup{
		{"Concurrency", slices.Concat(concurrencyCases, claimContentionCases, taskConcurrencyCases, trustBucketCases)},
		{"Publication", publicationCases},
		{"Replay", slices.Concat(replayCases, heldExpiryCases, snoozeWakeCases, heldDecisionCases)},
		{"Faults", faultCases},
	}
}

func runDurableGroups(t *testing.T, factory StoreFactory, restart RestartFunc, groups []durableGroup) {
	t.Helper()
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					fx := factory(t)
					if fx.Store == nil || fx.Now == nil {
						t.Fatal("StoreFixture needs Store and Now")
					}
					if restart != nil {
						if err := durableSuiteError(fx.Store.Properties(), restart); err != nil {
							t.Fatal(err)
						}
					}
					e := newEnv(t, fx)
					cross := func(*testing.T) {}
					if g.name == "Restart" {
						cross = func(t *testing.T) { t.Helper(); e.restart(t, restart) }
					}
					c.run(t, e, cross)
				})
			}
		})
	}
}

// restart replaces the env's store with one reopened on the same persisted
// state.
func (e *env) restart(t *testing.T, restart RestartFunc) {
	t.Helper()
	next := restart(t, e.fx)
	switch {
	case next.Store == nil || next.Now == nil:
		t.Fatal("RestartFunc returned a fixture without Store or Now")
	case next.Store == e.s:
		t.Fatal("RestartFunc returned the same Store; it must reopen persisted state in a new handle")
	case !next.Store.Properties().Durable:
		t.Fatal("restarted store reports Durable=false")
	}
	e.fx, e.s = next, next.Store
}

// heldUnit is a unit of work in tenantA held open after its writes, so a case
// can observe the store while the unit is in flight.
type heldUnit struct {
	decide   chan bool
	done     chan error
	once     sync.Once
	finished error
}

// holdUnit starts a unit on the env's current store that runs write and then
// waits, uncommitted, until finish is called. It fails the test when write
// fails. Cleanup aborts a unit the case never finished.
func (e *env) holdUnit(t *testing.T, write func(store.Tx) error) *heldUnit {
	t.Helper()
	u := &heldUnit{decide: make(chan bool), done: make(chan error, 1)}
	ready := make(chan error, 1)
	s := e.s
	go func() {
		u.done <- s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
			if err := write(tx); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			if <-u.decide {
				return nil
			}
			return errAbort
		})
	}()
	select {
	case err := <-ready:
		if err != nil {
			u.once.Do(func() { u.finished = <-u.done })
			t.Fatalf("held unit write: %v", err)
		}
	case err := <-u.done:
		u.once.Do(func() { u.finished = err })
		t.Fatalf("held unit ended before its writes: %v", err)
	}
	t.Cleanup(func() { _ = u.finish(false) })
	return u
}

// finish commits (commit=true) or aborts the held unit and returns Transact's
// result. Later calls return the first result.
func (u *heldUnit) finish(commit bool) error {
	u.once.Do(func() {
		u.decide <- commit
		u.finished = <-u.done
	})
	return u.finished
}

// claimIngress claims rec in its own unit and reports whether it was created.
func (e *env) claimIngress(t *testing.T, rec store.IngressRecord) (store.IngressRecord, bool) {
	t.Helper()
	var got store.IngressRecord
	var created bool
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimIngress(e.ctx, rec)
		return err
	})
	return got, created
}

func (e *env) ingressRecord(t *testing.T, dedup string) store.IngressRecord {
	t.Helper()
	return store.IngressRecord{ID: e.id(t), Channel: e.channel, DedupKey: dedup, VerifierVersion: "v1"}
}

func eventIDs(evs []model.Event) []model.ID {
	out := make([]model.ID, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}
