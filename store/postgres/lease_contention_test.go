package postgres_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// leaseRace runs one AcquireLease unit per resource concurrently, once
// each, on stores taken in turn from procs, and returns each unit's result.
func leaseRace(t *testing.T, procs []*postgres.Store, res []model.Ref, until time.Time) ([]store.Lease, []error) {
	t.Helper()
	ctx := t.Context()
	leases, errs := make([]store.Lease, len(res)), make([]error, len(res))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range res {
		st, owner := procs[i%len(procs)], fmt.Sprintf("receiver-%d", i)
		wg.Go(func() {
			<-start
			errs[i] = st.Transact(ctx, tenant, func(tx store.Tx) error {
				var err error
				leases[i], err = tx.AcquireLease(ctx, store.LeaseRequest{Resource: res[i], Owner: owner, Until: until})
				return err
			})
		})
	}
	close(start)
	wg.Wait()
	return leases, errs
}

// TestLeaseTakesOfDistinctResourcesDoNotAbort: channels starting at once
// in several processes each take their own receiver lease in one unit run
// once, with no rerun. On a small uma_leases table every key shares one
// index page; a take that read the lease row before writing it left a
// predicate lock on that page, so takes of different resources failed
// each other with model.Transient.
func TestLeaseTakesOfDistinctResourcesDoNotAbort(t *testing.T) {
	procs, clock := processes(t, 4)
	const channels, rounds = 8, 10
	for round := range rounds {
		res := make([]model.Ref, channels)
		for i := range res {
			res[i] = model.ChannelRef{Tenant: tenant, ID: model.ID(fmt.Sprintf("0000000e-0000-7000-8000-%06x%06x", round, i))}.Ref()
		}
		leases, errs := leaseRace(t, procs, res, clock.Now().Add(time.Minute))
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: take of channel %d = %v, want the lease", round, i, err)
			}
			if leases[i].Fence.Generation != 1 || leases[i].Resource != res[i] {
				t.Fatalf("round %d: channel %d lease = %+v, want generation 1 on its resource", round, i, leases[i])
			}
		}
	}
}

// TestLeaseTakeoverOfExpiredLeaseIsExclusive: concurrent takes of one
// resource whose lease expired grant it once, under the next generation;
// every other take is model.Conflict once its lost unit is rerun, as the
// engine reruns it.
func TestLeaseTakeoverOfExpiredLeaseIsExclusive(t *testing.T) {
	procs, clock := processes(t, 4)
	ctx := t.Context()
	for round := range 5 {
		res := model.ChannelRef{Tenant: tenant, ID: model.ID(fmt.Sprintf("0000000f-0000-7000-8000-%012x", round))}.Ref()
		if err := procs[0].Transact(ctx, tenant, func(tx store.Tx) error {
			_, err := tx.AcquireLease(ctx, store.LeaseRequest{Resource: res, Owner: "crashed", Until: clock.Now().Add(time.Second)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Second)
		const takers = 8
		var holders []store.Lease
		var mu sync.Mutex
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range takers {
			st, owner := procs[i%len(procs)], fmt.Sprintf("taker-%d", i)
			wg.Go(func() {
				<-start
				var l store.Lease
				err := retry(ctx, st, func(tx store.Tx) error {
					var err error
					l, err = tx.AcquireLease(ctx, store.LeaseRequest{Resource: res, Owner: owner, Until: clock.Now().Add(time.Minute)})
					return err
				})
				switch {
				case err == nil:
					mu.Lock()
					holders = append(holders, l)
					mu.Unlock()
				case !model.IsCode(err, model.Conflict):
					t.Errorf("take by %s = %v, want the lease or conflict", owner, err)
				}
			})
		}
		close(start)
		wg.Wait()
		if len(holders) != 1 || holders[0].Fence.Generation != 2 {
			t.Fatalf("round %d: holders = %+v, want exactly one at generation 2", round, holders)
		}
	}
}
