package conformance

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Concurrent claims across goroutines. Each worker's first unit waits for
// the others before it ends, so the units genuinely overlap. Units retry
// model.Transient serialization failures as the engine would; every other
// outcome counts.
var concurrencyCases = []durableCase{
	{"FirstContactClaimsConverge", func(t *testing.T, e *env, _ func(*testing.T)) {
		// The first-contact unit: claim the identity, write its provisional
		// contact, claim the conversation and append its creation event.
		// Concurrent first messages from one sender converge on one of each.
		key := e.identityKey("U-first-contact")
		sel := store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerContact, Key: "first-contact"}
		const workers = 8
		type result struct {
			identity       model.IdentityRef
			conversation   model.ConversationRef
			newID, newConv bool
		}
		results := make([]result, workers)
		meet := newRendezvous(workers)
		var wg sync.WaitGroup
		for i := range workers {
			cand := e.identity(t, key)
			conv := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
			ev := e.event(t, "conformance.conversation.created")
			ev.Resource = conv.Ref.Ref()
			wg.Go(func() {
				var r result
				err := e.overlapTx(meet, func(tx store.Tx) error {
					r = result{}
					id, created, err := tx.ClaimIdentity(e.ctx, key, cand)
					if err != nil {
						return err
					}
					if created {
						if err := tx.PutContact(e.ctx, model.Contact{Ref: cand.Contact, Kind: model.ContactKindPerson, Revision: "c1"}, ""); err != nil {
							return err
						}
					}
					c, convCreated, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: "first", Conversation: conv})
					if err != nil {
						return err
					}
					if convCreated {
						if err := tx.AppendEvents(e.ctx, []model.Event{ev}); err != nil {
							return err
						}
					}
					r = result{identity: id.Ref, conversation: c.Ref, newID: created, newConv: convCreated}
					return nil
				})
				if err != nil {
					t.Errorf("first-contact unit: %v", err)
				}
				results[i] = r
			})
		}
		wg.Wait()
		var winners int
		for i, r := range results {
			if r.newID != r.newConv {
				t.Errorf("unit %d created identity=%v conversation=%v; one unit must create both", i, r.newID, r.newConv)
			}
			if r.newID {
				winners++
			}
			if r.identity != results[0].identity || r.conversation != results[0].conversation {
				t.Fatalf("units disagree: %+v vs %+v", r, results[0])
			}
		}
		if winners != 1 {
			t.Fatalf("%d units created the first contact, want 1", winners)
		}
		e.mustView(t, func(rd store.Reader) error {
			contacts, err := rd.Contacts(e.ctx, list(100))
			if err != nil {
				return err
			}
			if len(contacts.Items) != 1 {
				t.Errorf("contacts = %d, want 1 and no orphans from losing units", len(contacts.Items))
			}
			cands, err := rd.Candidates(e.ctx, sel, 10)
			if err != nil {
				return err
			}
			if len(cands) != 1 || cands[0].Ref != results[0].conversation {
				t.Errorf("candidates = %d, want only the claimed conversation", len(cands))
			}
			return nil
		})
		if evs := e.allEvents(t, model.Filter{}, 10); len(evs) != 1 {
			t.Errorf("events = %d, want 1 creation event", len(evs))
		}
	}},
	{"IngressClaimsConverge", func(t *testing.T, e *env, _ func(*testing.T)) {
		// A provider redelivering one event to several receivers at once.
		const workers = 8
		recs := make([]store.IngressRecord, workers)
		got := make([]store.IngressRecord, workers)
		created := make([]bool, workers)
		for i := range recs {
			recs[i] = e.ingressRecord(t, "evt-race")
		}
		meet := newRendezvous(workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				err := e.overlapTx(meet, func(tx store.Tx) error {
					var err error
					got[i], created[i], err = tx.ClaimIngress(e.ctx, recs[i])
					return err
				})
				if err != nil {
					t.Errorf("claim: %v", err)
				}
			})
		}
		wg.Wait()
		winner := -1
		for i := range workers {
			if created[i] {
				if winner >= 0 {
					t.Fatalf("units %d and %d both created the ingress record", winner, i)
				}
				winner = i
			}
		}
		if winner < 0 {
			t.Fatal("no unit created the ingress record")
		}
		for i := range workers {
			if got[i].ID != recs[winner].ID {
				t.Errorf("unit %d got ingress %s, want the winner %s", i, got[i].ID, recs[winner].ID)
			}
		}
		e.mustView(t, func(r store.Reader) error {
			for i, rec := range recs {
				_, err := r.Ingress(e.ctx, rec.ID)
				if i == winner && err != nil {
					return err
				}
				if i != winner {
					wantCode(t, err, model.NotFound)
				}
			}
			return nil
		})
	}},
	{"OperationClaimsConverge", func(t *testing.T, e *env, _ func(*testing.T)) {
		// Concurrent submissions of one operation ID: equal payloads replay
		// the one stored claim, a different payload conflicts, whichever
		// claim commits first.
		const same, other = 8, 4
		recs := make([]store.OperationRecord, same+other)
		for i := range recs {
			fp := []byte{1}
			if i >= same {
				fp = []byte{2}
			}
			recs[i] = e.operation(t, "op-race", fp)
		}
		got := make([]store.OperationRecord, len(recs))
		created := make([]bool, len(recs))
		errs := make([]error, len(recs))
		meet := newRendezvous(len(recs))
		var wg sync.WaitGroup
		for i := range recs {
			wg.Go(func() {
				errs[i] = e.overlapTx(meet, func(tx store.Tx) error {
					var err error
					got[i], created[i], err = tx.ClaimOperation(e.ctx, recs[i])
					return err
				})
			})
		}
		wg.Wait()
		winner := -1
		for i := range recs {
			if created[i] && errs[i] == nil {
				if winner >= 0 {
					t.Fatalf("units %d and %d both created the operation", winner, i)
				}
				winner = i
			}
		}
		if winner < 0 {
			t.Fatalf("no unit created the operation: %v", errs)
		}
		for i, rec := range recs {
			if bytes.Equal(rec.Fingerprint, recs[winner].Fingerprint) {
				if errs[i] != nil || got[i].Operation.Ref != recs[winner].Operation.Ref {
					t.Errorf("equal payload %d = %v, %v; want replay of %v", i, got[i].Operation.Ref, errs[i], recs[winner].Operation.Ref)
				}
				continue
			}
			wantCode(t, errs[i], model.Conflict)
		}
		e.mustView(t, func(r store.Reader) error {
			for i, rec := range recs {
				_, err := r.Operation(e.ctx, rec.Operation.Ref)
				if i == winner && err != nil {
					return err
				}
				if i != winner {
					wantCode(t, err, model.NotFound)
				}
			}
			return nil
		})
	}},
	{"WorkClaimsAreExclusive", func(t *testing.T, e *env, _ func(*testing.T)) {
		// Workers draining one queue concurrently: every item is leased to
		// exactly one worker while its lease is live.
		const items, workers = 12, 4
		e.mustTx(t, func(tx store.Tx) error {
			for range items {
				if err := tx.EnqueueWork(e.ctx, e.work(t)); err != nil {
					return err
				}
			}
			return nil
		})
		var mu sync.Mutex
		claimed := map[model.ID][]store.Work{}
		meet := newRendezvous(workers)
		var wg sync.WaitGroup
		for w := range workers {
			owner := "worker-" + string(rune('a'+w))
			wg.Go(func() {
				for range items + 1 {
					var got []store.Work
					err := e.overlapTx(meet, func(tx store.Tx) error {
						var err error
						got, err = tx.ClaimWork(e.ctx, store.WorkClaim{
							Kinds: []string{workKind}, Limit: 2, Owner: owner, Until: e.fx.Now().Add(time.Minute),
						})
						return err
					})
					if err != nil {
						t.Errorf("%s claim: %v", owner, err)
						return
					}
					if len(got) == 0 {
						return
					}
					mu.Lock()
					for _, w := range got {
						claimed[w.ID] = append(claimed[w.ID], w)
					}
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if len(claimed) != items {
			t.Fatalf("claimed %d distinct items, want %d", len(claimed), items)
		}
		for id, ws := range claimed {
			if len(ws) != 1 {
				t.Errorf("item %s leased %d times while its lease was live", id, len(ws))
			}
		}
		e.mustTx(t, func(tx store.Tx) error {
			for _, ws := range claimed {
				if err := tx.CompleteWork(e.ctx, ws[0]); err != nil {
					return err
				}
			}
			return nil
		})
		if rest := e.claimWork(t, "worker-z", 10); len(rest) != 0 {
			t.Fatalf("completed work claimable again: %v", workIDs(rest))
		}
	}},
	{"LeaseAcquireIsExclusive", func(t *testing.T, e *env, _ func(*testing.T)) {
		res := model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}.Ref()
		const workers = 8
		leases := make([]store.Lease, workers)
		errs := make([]error, workers)
		meet := newRendezvous(workers)
		var wg sync.WaitGroup
		for i := range workers {
			owner := "owner-" + string(rune('a'+i))
			wg.Go(func() {
				errs[i] = e.overlapTx(meet, func(tx store.Tx) error {
					var err error
					leases[i], err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: owner, Until: e.fx.Now().Add(time.Minute)})
					return err
				})
			})
		}
		wg.Wait()
		holders := 0
		for i := range workers {
			if errs[i] == nil {
				holders++
				continue
			}
			wantCode(t, errs[i], model.Conflict)
		}
		if holders != 1 {
			t.Fatalf("%d concurrent acquirers hold the lease, want 1", holders)
		}
	}},
	{"StreamSequenceAdvancesOnce", func(t *testing.T, e *env, _ func(*testing.T)) {
		// Concurrent updates of one stream, each advancing the sequence it
		// read under that read's revision. A unit that lost the race retries
		// from the new state, as the engine does, so no advance is lost or
		// applied twice.
		conv := e.putConversation(t)
		ref := model.StreamRef{Conversation: conv, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			return tx.PutStream(e.ctx, model.Stream{
				Ref: ref, Mode: model.StreamNative, State: model.StreamOpen, NextSequence: 1, Revision: "s0",
			}, "")
		})
		const workers = 8
		ops := make([]model.OperationRef, workers)
		for i := range ops {
			ops[i] = model.OperationRef{Tenant: tenantA, ID: e.id(t)}
		}
		errs := make([]error, workers)
		meet := newRendezvous(workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				attempt := 0
				errs[i] = e.overlapTx(meet, func(tx store.Tx) error {
					attempt++
					cur, err := tx.Stream(e.ctx, ref)
					if err != nil {
						return err
					}
					prev := cur.Revision
					cur.NextSequence++
					cur.LastOperation = &ops[i]
					cur.Revision = model.Revision(fmt.Sprintf("s-%d-%d", i, attempt))
					if err := tx.PutStream(e.ctx, cur, prev); model.IsCode(err, model.Conflict) {
						return model.NewError(model.Transient, "stream changed concurrently")
					} else if err != nil {
						return err
					}
					return nil
				})
			})
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("update %d: %v", i, err)
			}
		}
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Stream(e.ctx, ref)
			if err != nil {
				return err
			}
			if got.NextSequence != 1+workers {
				t.Errorf("next sequence = %d after %d updates, want %d", got.NextSequence, workers, 1+workers)
			}
			if got.LastOperation == nil || !slices.Contains(ops, *got.LastOperation) {
				t.Errorf("last operation = %v, want one of the updates", got.LastOperation)
			}
			return nil
		})
	}},
}

// overlapWait bounds how long a unit waits for the other workers. A backend
// whose writers exclude each other blocks the others meanwhile, so the wait
// ends by timeout there instead of deadlocking.
const overlapWait = 100 * time.Millisecond

// rendezvous lets n workers' units overlap: each waits until all n arrived
// or overlapWait passed.
type rendezvous struct {
	mu      sync.Mutex
	n       int
	arrived int
	all     chan struct{}
}

func newRendezvous(n int) *rendezvous { return &rendezvous{n: n, all: make(chan struct{})} }

func (r *rendezvous) wait() {
	r.mu.Lock()
	r.arrived++
	if r.arrived == r.n {
		close(r.all)
	}
	r.mu.Unlock()
	timer := time.NewTimer(overlapWait)
	defer timer.Stop()
	select {
	case <-r.all:
	case <-timer.C:
	}
}

// overlapTx is retryTx whose first attempt runs fn and then waits at meet
// before the unit ends.
func (e *env) overlapTx(meet *rendezvous, fn func(store.Tx) error) error {
	first := true
	return e.retryTx(func(tx store.Tx) error {
		err := fn(tx)
		if first {
			first = false
			meet.wait()
		}
		return err
	})
}
