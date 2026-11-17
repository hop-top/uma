package postgres_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

const workKind = "postgres.test.work"

// processes returns n stores on one database, each on its own pool, as n
// processes sharing the database hold them. They share one clock.
func processes(t *testing.T, n int) ([]*postgres.Store, *portstest.FixedClock) {
	t.Helper()
	first := postgres.NewTestDatabase(t, true)
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	out := make([]*postgres.Store, n)
	for i := range out {
		pool := first
		if i > 0 {
			pool = postgres.NewTestPool(t, first)
		}
		st, err := postgres.Open(t.Context(), pool, postgres.Options{Clock: clock})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		out[i] = st
	}
	return out, clock
}

// roundRobin spreads units over stores on separate pools, so units the
// durable cases run concurrently land in different processes.
type roundRobin struct {
	stores []*postgres.Store
	n      atomic.Uint64
}

func (r *roundRobin) next() *postgres.Store {
	return r.stores[(r.n.Add(1)-1)%uint64(len(r.stores))]
}

func (r *roundRobin) Properties() store.Properties { return r.stores[0].Properties() }

func (r *roundRobin) View(ctx context.Context, tenant model.TenantID, fn func(store.Reader) error) error {
	return r.next().View(ctx, tenant, fn)
}

func (r *roundRobin) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return r.next().Transact(ctx, tenant, fn)
}

// TestDurableLogicalAcrossPools runs the concurrency, publication and replay
// cases with consecutive units alternating between two pools: concurrent
// first contact, claims, leases and tails converge across processes.
func TestDurableLogicalAcrossPools(t *testing.T) {
	conformance.RunDurableLogicalSuite(t, func(t *testing.T) conformance.StoreFixture {
		procs, clock := processes(t, 2)
		return conformance.StoreFixture{
			Store:   &roundRobin{stores: procs},
			Now:     clock.Now,
			Advance: func(d time.Duration) { clock.Advance(d) },
		}
	})
}

// retry runs a unit on st until it ends with anything but a serialization
// failure, as the engine retries.
func retry(ctx context.Context, st *postgres.Store, fn func(store.Tx) error) error {
	for {
		if err := st.Transact(ctx, tenant, fn); !model.IsCode(err, model.Transient) {
			return err
		}
	}
}

func enqueue(t *testing.T, st *postgres.Store, n int) []store.Work {
	t.Helper()
	ctx := t.Context()
	ids := portstest.NewSequenceIDs(11)
	id := func() model.ID {
		v, err := ids.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	out := make([]store.Work, n)
	for i := range out {
		out[i] = store.Work{ID: id(), Kind: workKind, Resource: model.OperationRef{Tenant: tenant, ID: id()}.Ref()}
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		for _, w := range out {
			if err := tx.EnqueueWork(ctx, w); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func claim(ctx context.Context, tx store.Tx, owner string, limit int, until time.Time) ([]store.Work, error) {
	return tx.ClaimWork(ctx, store.WorkClaim{Kinds: []string{workKind}, Limit: limit, Owner: owner, Until: until})
}

// TestWorkClaimsAcrossPools: workers in two processes drain one queue
// concurrently. Every item is leased exactly once while its lease is live;
// once leases expire, another process reclaims items under a higher
// generation and the stale fence no longer completes them.
func TestWorkClaimsAcrossPools(t *testing.T) {
	ctx := t.Context()
	procs, clock := processes(t, 2)
	const items, workers = 60, 8
	enqueue(t, procs[0], items)
	until := clock.Now().Add(time.Minute)
	var mu sync.Mutex
	claimed := map[model.ID][]store.Work{}
	var total atomic.Int64
	deadline := time.Now().Add(time.Minute)
	var wg sync.WaitGroup
	for w := range workers {
		st := procs[w%len(procs)]
		owner := "worker-" + string(rune('a'+w))
		wg.Go(func() {
			// A claim may come back empty while other units hold the
			// remaining items; those units commit or retry.
			for total.Load() < items && time.Now().Before(deadline) {
				var got []store.Work
				if err := retry(ctx, st, func(tx store.Tx) error {
					var err error
					got, err = claim(ctx, tx, owner, 2, until)
					return err
				}); err != nil {
					t.Errorf("%s claim: %v", owner, err)
					return
				}
				mu.Lock()
				for _, w := range got {
					claimed[w.ID] = append(claimed[w.ID], w)
				}
				mu.Unlock()
				total.Add(int64(len(got)))
			}
		})
	}
	wg.Wait()
	for id, ws := range claimed {
		if len(ws) != 1 {
			t.Fatalf("item %s leased %d times while its lease was live: %+v", id, len(ws), ws)
		}
	}
	if len(claimed) != items {
		t.Fatalf("claimed %d distinct items, want %d", len(claimed), items)
	}

	clock.Advance(2 * time.Minute)
	var again []store.Work
	if err := retry(ctx, procs[1], func(tx store.Tx) error {
		var err error
		again, err = claim(ctx, tx, "worker-late", items, clock.Now().Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(again) != items {
		t.Fatalf("reclaimed %d expired items, want %d", len(again), items)
	}
	for _, cur := range again {
		old := claimed[cur.ID][0]
		if cur.Fence.Generation <= old.Fence.Generation {
			t.Fatalf("item %s reclaimed under generation %d, want above %d", cur.ID, cur.Fence.Generation, old.Fence.Generation)
		}
		err := procs[0].Transact(ctx, tenant, func(tx store.Tx) error { return tx.CompleteWork(ctx, old) })
		if model.CodeOf(err) != model.Conflict {
			t.Fatalf("complete under the stale fence = %v, want conflict", err)
		}
	}
	if err := retry(ctx, procs[0], func(tx store.Tx) error {
		for _, w := range again {
			if err := tx.CompleteWork(ctx, w); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("complete under the current fences: %v", err)
	}
}

// TestClaimSkipsItemsInFlight: a claim neither waits for nor returns the
// items another process's unit is claiming; it takes the next ones.
func TestClaimSkipsItemsInFlight(t *testing.T) {
	ctx := t.Context()
	procs, clock := processes(t, 2)
	ws := enqueue(t, procs[0], 3)
	until := clock.Now().Add(time.Minute)

	held := make(chan []store.Work, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		first := true
		done <- retry(ctx, procs[0], func(tx store.Tx) error {
			got, err := claim(ctx, tx, "holder", 1, until)
			if first {
				first = false
				held <- got
				<-release
			}
			return err
		})
	}()
	var first []store.Work
	select {
	case first = <-held:
	case err := <-done:
		t.Fatalf("holding unit ended early: %v", err)
	}
	if len(first) != 1 || first[0].ID != ws[0].ID {
		close(release)
		t.Fatalf("holder claimed %v, want the oldest item", first)
	}

	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	var seen []model.ID
	err := procs[1].Transact(wait, tenant, func(tx store.Tx) error {
		got, err := claim(wait, tx, "other", 1, until)
		for _, w := range got {
			seen = append(seen, w.ID)
		}
		return err
	})
	cancel()
	close(release)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		t.Fatal("claim waited for the in-flight unit's items")
	case err != nil && !model.IsCode(err, model.Transient):
		t.Fatalf("concurrent claim: %v", err)
	}
	if !slices.Equal(seen, []model.ID{ws[1].ID}) {
		t.Fatalf("concurrent claim saw %v, want only the next item %s", seen, ws[1].ID)
	}
	if err := <-done; err != nil {
		t.Fatalf("holding unit: %v", err)
	}
}
