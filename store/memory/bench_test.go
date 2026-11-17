package memory

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// BenchmarkClaimWork drains a queue of b.N work items, one resource each,
// with concurrent claimers on one memory store: each claims one item per
// unit, completes it in a second unit and reruns any unit that failed with
// model.Transient, as the engine does. ns/op is wall time per completed
// item. attempts/claim counts claim units per item claimed, so 1 means no
// conflict; over3-% is the share of claims that needed more than three
// units, the engine's per-round bound. in_flight=16 applies the engine's
// default MaxInFlight cap. lost-completes counts completions refused
// because another claim held or had finished the item: zero while claims
// are exclusive.
func BenchmarkClaimWork(b *testing.B) {
	for _, inFlight := range []int{0, 16} {
		for _, n := range []int{1, 4, 8, 16} {
			b.Run(fmt.Sprintf("in_flight=%d/claimers=%d", inFlight, n), func(b *testing.B) {
				clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
				st := New(Options{Clock: clock})
				benchClaims(b, st, clock.Now, n, inFlight)
			})
		}
	}
}

const benchTenant model.TenantID = "bench-tenant"

const benchKind = "bench.work"

// benchLease outlasts any stall of a loaded machine, so a lease expiring
// between claim and completion never reads as contention.
const benchLease = 10 * time.Minute

func benchClaims(b *testing.B, st store.Store, now func() time.Time, claimers, maxInFlight int) {
	ctx := b.Context()
	for lo := 0; lo < b.N; lo += 500 {
		if err := st.Transact(ctx, benchTenant, func(tx store.Tx) error {
			for i := lo; i < min(lo+500, b.N); i++ {
				w := store.Work{
					ID: benchID(1, i), Kind: benchKind,
					Resource: model.OperationRef{Tenant: benchTenant, ID: benchID(2, i)}.Ref(),
				}
				if err := tx.EnqueueWork(ctx, w); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("enqueue: %v", err)
		}
	}
	var done, claimUnits, claims, over3, completeRetries, lost atomic.Int64
	var failed atomic.Bool
	b.ResetTimer()
	var wg sync.WaitGroup
	for c := range claimers {
		owner := fmt.Sprintf("claimer-%d", c)
		wg.Go(func() {
			for done.Load() < int64(b.N) && !failed.Load() {
				var got []store.Work
				units := 0
				for {
					units++
					err := st.Transact(ctx, benchTenant, func(tx store.Tx) error {
						var err error
						got, err = tx.ClaimWork(ctx, store.WorkClaim{
							Kinds: []string{benchKind}, Limit: 1, Owner: owner,
							Until: now().Add(benchLease), MaxInFlight: maxInFlight,
						})
						return err
					})
					if model.IsCode(err, model.Transient) || model.IsCode(err, model.Unavailable) {
						continue
					}
					if err != nil {
						failed.Store(true)
						b.Errorf("%s claim: %v", owner, err)
						return
					}
					break
				}
				if len(got) == 0 {
					continue
				}
				claimUnits.Add(int64(units))
				claims.Add(1)
				if units > 3 {
					over3.Add(1)
				}
				for {
					err := st.Transact(ctx, benchTenant, func(tx store.Tx) error { return tx.CompleteWork(ctx, got[0]) })
					if model.IsCode(err, model.Transient) {
						completeRetries.Add(1)
						continue
					}
					if model.IsCode(err, model.Conflict) || model.IsCode(err, model.NotFound) {
						// Another claim holds or finished the item.
						lost.Add(1)
						break
					}
					if err != nil {
						failed.Store(true)
						b.Errorf("%s complete: %v", owner, err)
						return
					}
					done.Add(1)
					break
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	if n := claims.Load(); n > 0 {
		b.ReportMetric(float64(claimUnits.Load())/float64(n), "attempts/claim")
		b.ReportMetric(100*float64(over3.Load())/float64(n), "over3-%")
		b.ReportMetric(float64(completeRetries.Load())/float64(n), "complete-retries/item")
		b.ReportMetric(float64(lost.Load()), "lost-completes")
	}
}

// benchID is a well-formed internal ID unique per (space, i).
func benchID(space, i int) model.ID {
	return model.ID(fmt.Sprintf("%08x-0000-7000-8000-%012x", space, i))
}
