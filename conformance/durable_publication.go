package conformance

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Commit versus publication ordering. While a unit is in flight, the cases
// observe the store through View only, so a backend whose writers exclude
// each other cannot deadlock the case.
var publicationCases = []durableCase{
	{"InFlightUnitIsInvisibleUntilCommit", func(t *testing.T, e *env, _ func(*testing.T)) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		ev := e.event(t, "conformance.published")
		w := e.work(t)
		in := e.ingressRecord(t, "evt-in-flight")
		u := e.holdUnit(t, e.publishingWrite(ch, ev, w, in))
		e.requireUnpublished(t, ch, in)
		if err := u.finish(true); err != nil {
			t.Fatalf("held unit commit: %v", err)
		}
		e.mustView(t, func(r store.Reader) error {
			if _, err := r.GetChannel(e.ctx, ch); err != nil {
				return err
			}
			_, err := r.Ingress(e.ctx, in.ID)
			return err
		})
		if got := eventIDs(e.allEvents(t, model.Filter{}, 10)); len(got) != 1 || got[0] != ev.ID {
			t.Fatalf("events after commit = %v, want [%s]", got, ev.ID)
		}
		if got := e.claimWork(t, "publisher", 10); len(got) != 1 || got[0].ID != w.ID {
			t.Fatalf("publication work after commit = %v, want [%s]", workIDs(got), w.ID)
		}
	}},
	{"AbortedInFlightUnitPublishesNothing", func(t *testing.T, e *env, _ func(*testing.T)) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		in := e.ingressRecord(t, "evt-aborted")
		u := e.holdUnit(t, e.publishingWrite(ch, e.event(t, "conformance.lost"), e.work(t), in))
		e.requireUnpublished(t, ch, in)
		if err := u.finish(false); !errors.Is(err, errAbort) {
			t.Fatalf("held unit abort = %v, want the callback error", err)
		}
		e.requireUnpublished(t, ch, in)
		if got := e.claimWork(t, "publisher", 10); len(got) != 0 {
			t.Fatalf("aborted unit's work is claimable: %v", workIDs(got))
		}
		if _, created := e.claimIngress(t, e.ingressRecord(t, "evt-aborted")); !created {
			t.Fatal("aborted unit's ingress claim still holds its dedup key")
		}
	}},
	{"TailSeesEveryCommitOnceInOrder", func(t *testing.T, e *env, _ func(*testing.T)) {
		// Writers commit concurrently while a reader tails the log from its
		// last cursor. The tail must see every committed event exactly once,
		// with no gap left behind by a unit that committed late, and each
		// unit's events adjacent and in append order.
		const writers, units, perUnit = 4, 5, 2
		var unitsMu sync.Mutex
		var committed [][]model.ID
		var writing atomic.Int64
		writing.Store(writers)
		var wg sync.WaitGroup
		for range writers {
			batches := make([][]model.Event, units)
			for u := range batches {
				for range perUnit {
					batches[u] = append(batches[u], e.event(t, "conformance.tail"))
				}
			}
			wg.Go(func() {
				defer writing.Add(-1)
				for _, evs := range batches {
					if err := e.retryTx(func(tx store.Tx) error { return tx.AppendEvents(e.ctx, evs) }); err != nil {
						t.Errorf("append: %v", err)
						return
					}
					unitsMu.Lock()
					committed = append(committed, eventIDs(evs))
					unitsMu.Unlock()
				}
			})
		}
		// A failing read below must not end the case while writers run.
		defer wg.Wait()
		var tail []model.ID
		var after model.Cursor
		for {
			done := writing.Load() == 0
			for {
				var p model.Page[model.Event]
				e.mustView(t, func(r store.Reader) error {
					var err error
					p, err = r.Events(e.ctx, model.Filter{}, after, 3)
					return err
				})
				tail = append(tail, eventIDs(p.Items)...)
				if p.Next != "" {
					after = p.Next
				}
				if p.Complete {
					break
				}
			}
			if done {
				break
			}
		}
		wg.Wait()
		want := writers * units * perUnit
		seen := make(map[model.ID]int, len(tail))
		for i, id := range tail {
			if j, dup := seen[id]; dup {
				t.Fatalf("tail saw event %s at %d and %d", id, j, i)
			}
			seen[id] = i
		}
		if len(tail) != want {
			t.Fatalf("tail saw %d events, want %d committed", len(tail), want)
		}
		for _, ids := range committed {
			first, ok := seen[ids[0]]
			if !ok {
				t.Fatalf("tail missed committed event %s", ids[0])
			}
			for k, id := range ids {
				if seen[id] != first+k {
					t.Fatalf("unit events %v not adjacent in append order in the tail", ids)
				}
			}
		}
		if full := eventIDs(e.allEvents(t, model.Filter{}, 7)); !slices.Equal(full, tail) {
			t.Fatal("a full read disagrees with the tail's order")
		}
	}},
}

// publishingWrite records a domain change together with its event,
// publication work and ingress claim, as one ingress unit does.
func (e *env) publishingWrite(ch model.ChannelRef, ev model.Event, w store.Work, in store.IngressRecord) func(store.Tx) error {
	return func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""); err != nil {
			return err
		}
		if err := tx.AppendEvents(e.ctx, []model.Event{ev}); err != nil {
			return err
		}
		if err := tx.EnqueueWork(e.ctx, w); err != nil {
			return err
		}
		_, _, err := tx.ClaimIngress(e.ctx, in)
		return err
	}
}

// requireUnpublished checks through View that none of a publishing write is
// visible.
func (e *env) requireUnpublished(t *testing.T, ch model.ChannelRef, in store.IngressRecord) {
	t.Helper()
	e.requireAbsent(t, ch)
	e.requireNoEvents(t)
	err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
		_, err := r.Ingress(e.ctx, in.ID)
		return err
	})
	wantCode(t, err, model.NotFound)
}
