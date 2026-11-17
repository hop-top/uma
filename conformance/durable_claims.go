package conformance

import (
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Contended claims: many claimers in overlapping units, each round's units
// meeting before they end. A backend may run claim units under any
// isolation as long as these hold; they are the cases a claim that checks
// a lease in one statement and takes it in another fails.
var claimContentionCases = []durableCase{
	{"WorkClaimsLeaseEachResourceOnce", func(t *testing.T, e *env, _ func(*testing.T)) {
		// Two queued items per resource, adjacent in the queue, so one
		// claimer's item is the next one a concurrent claimer reaches.
		// Nothing completes and no lease expires: each resource is leased
		// once, by one claim of one of its items.
		const resources, claimers, rounds = 40, 8, 5
		owner := map[model.ID]model.Ref{}
		e.mustTx(t, func(tx store.Tx) error {
			for range resources {
				res := model.OperationRef{Tenant: tenantA, ID: e.id(t)}.Ref()
				for range 2 {
					w := store.Work{ID: e.id(t), Kind: workKind, Resource: res}
					owner[w.ID] = res
					if err := tx.EnqueueWork(e.ctx, w); err != nil {
						return err
					}
				}
			}
			return nil
		})
		var mu sync.Mutex
		grants := map[model.Ref][]store.Work{}
		for round := range rounds {
			meet := newRendezvous(claimers)
			var wg sync.WaitGroup
			for c := range claimers {
				name := "claimer-" + string(rune('a'+c))
				wg.Go(func() {
					var got []store.Work
					err := e.overlapTx(meet, func(tx store.Tx) error {
						var err error
						got, err = tx.ClaimWork(e.ctx, store.WorkClaim{
							Kinds: []string{workKind}, Limit: 1, Owner: name, Until: e.fx.Now().Add(time.Hour),
						})
						return err
					})
					if err != nil {
						t.Errorf("round %d %s claim: %v", round, name, err)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					for _, w := range got {
						if owner[w.ID] != w.Resource {
							t.Errorf("claimed item %s on resource %v, enqueued on %v", w.ID, w.Resource, owner[w.ID])
						}
						grants[w.Resource] = append(grants[w.Resource], w)
					}
				})
			}
			wg.Wait()
		}
		twice := 0
		for res, ws := range grants {
			if len(ws) > 1 {
				twice++
				t.Errorf("resource %v leased %d times while its lease was live: %v", res, len(ws), workIDs(ws))
			}
		}
		if twice > 0 {
			t.Fatalf("%d of %d resources leased twice at once", twice, resources)
		}
		if len(grants) != resources {
			t.Errorf("leased %d resources after %d rounds of %d claimers, want all %d", len(grants), rounds, claimers, resources)
		}
		if rest := e.claimWork(t, "claimer-z", 2*resources); len(rest) != 0 {
			t.Fatalf("items of leased resources claimable: %v", workIDs(rest))
		}
	}},
	{"InFlightCapHoldsUnderConcurrentClaimers", func(t *testing.T, e *env, _ func(*testing.T)) {
		// More claimers than the cap allows, nothing completed: exactly the
		// cap's worth of items is claimed, whatever the interleaving.
		const items, claimers, rounds, maxInFlight = 16, 8, 3, 4
		e.mustTx(t, func(tx store.Tx) error {
			for range items {
				if err := tx.EnqueueWork(e.ctx, e.work(t)); err != nil {
					return err
				}
			}
			return nil
		})
		var mu sync.Mutex
		var claimed []store.Work
		for round := range rounds {
			meet := newRendezvous(claimers)
			var wg sync.WaitGroup
			for c := range claimers {
				name := "claimer-" + string(rune('a'+c))
				wg.Go(func() {
					var got []store.Work
					err := e.overlapTx(meet, func(tx store.Tx) error {
						var err error
						got, err = tx.ClaimWork(e.ctx, store.WorkClaim{
							Kinds: []string{workKind}, Limit: 1, Owner: name, Until: e.fx.Now().Add(time.Hour),
							MaxInFlight: maxInFlight,
						})
						return err
					})
					if err != nil && !model.IsCode(err, model.Unavailable) {
						t.Errorf("round %d %s claim: %v", round, name, err)
						return
					}
					mu.Lock()
					claimed = append(claimed, got...)
					mu.Unlock()
				})
			}
			wg.Wait()
		}
		if len(claimed) != maxInFlight {
			t.Fatalf("claimed %d items under an in-flight cap of %d: %v", len(claimed), maxInFlight, workIDs(claimed))
		}
		e.mustView(t, func(r store.Reader) error {
			d, err := r.WorkDepth(e.ctx, workKind)
			if err == nil && d != (store.WorkDepth{Pending: items - maxInFlight, Claimed: maxInFlight}) {
				t.Errorf("depth = %+v, want %d pending and %d claimed", d, items-maxInFlight, maxInFlight)
			}
			return err
		})
	}},
}
