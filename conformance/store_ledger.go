package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

const workKind = "conformance.deliver"

var ledgerCases = []storeCase{
	{"WorkClaimIsFencedAndBounded", func(t *testing.T, e *env) {
		works := []store.Work{e.work(t), e.work(t), e.work(t)}
		e.mustTx(t, func(tx store.Tx) error {
			for _, w := range works {
				if err := tx.EnqueueWork(e.ctx, w); err != nil {
					return err
				}
			}
			return nil
		})
		got := e.claimWork(t, "worker-a", 2)
		if len(got) != 2 || got[0].ID != works[0].ID || got[1].ID != works[1].ID {
			t.Fatalf("claimed %v, want the first two in enqueue order", workIDs(got))
		}
		for _, w := range got {
			if w.Fence.Owner != "worker-a" || w.Fence.Generation == 0 {
				t.Errorf("work %s fence = %+v, want owner worker-a and a generation", w.ID, w.Fence)
			}
		}
		rest := e.claimWork(t, "worker-b", 10)
		if len(rest) != 1 || rest[0].ID != works[2].ID {
			t.Fatalf("second claim = %v, want only the unleased item", workIDs(rest))
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteWork(e.ctx, got[0]) })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.CompleteWork(e.ctx, got[0]) }), model.NotFound)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, err := tx.ClaimWork(e.ctx, store.WorkClaim{Kinds: []string{workKind}, Owner: "w", Until: e.fx.Now().Add(time.Minute)})
			return err
		}), model.Validation)
	}},
	{"WorkInRolledBackUnitIsNotClaimable", func(t *testing.T, e *env) {
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.EnqueueWork(e.ctx, e.work(t)); err != nil {
				return err
			}
			return errAbort
		})
		if got := e.claimWork(t, "worker-a", 10); len(got) != 0 {
			t.Fatalf("claimed %v from a rolled-back unit", workIDs(got))
		}
	}},
	{"EmptyClaimDoesNotInvalidateInFlightUnit", func(t *testing.T, e *env) {
		// An idle poll claims nothing and so writes nothing: a unit in
		// flight when it runs must still commit without a retry. The item
		// enqueued by the in-flight unit is not yet visible to the poll.
		w := e.work(t)
		err := e.txErr(func(tx store.Tx) error {
			if err := tx.EnqueueWork(e.ctx, w); err != nil {
				return err
			}
			if got := e.claimWork(t, "idle", 10); len(got) != 0 {
				t.Fatalf("idle claim returned %v, want none", workIDs(got))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unit in flight during an empty claim: %v, want it to commit", err)
		}
		if got := e.claimWork(t, "worker-a", 10); len(got) != 1 || got[0].ID != w.ID {
			t.Fatalf("claimed %v, want the enqueued item", workIDs(got))
		}
	}},
	{"ExpiredWorkIsReclaimedUnderNewGeneration", func(t *testing.T, e *env) {
		if e.fx.Advance == nil {
			t.Skip("fixture cannot advance the store clock")
		}
		w := e.work(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.EnqueueWork(e.ctx, w) })
		old := e.claimWork(t, "worker-a", 1)
		e.fx.Advance(2 * time.Minute)
		renewed := old[0]
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, err := tx.RenewLease(e.ctx, store.Lease{Resource: renewed.Resource, Fence: renewed.Fence, Until: e.fx.Now().Add(time.Minute)})
			return err
		}), model.Conflict)
		cur := e.claimWork(t, "worker-b", 1)
		if len(cur) != 1 || cur[0].Fence.Generation <= old[0].Fence.Generation {
			t.Fatalf("reclaim = %+v, want a higher generation than %+v", cur, old[0].Fence)
		}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.CompleteWork(e.ctx, old[0]) }), model.Conflict)
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteWork(e.ctx, cur[0]) })
	}},
	{"DeferredWorkWaitsForNotBefore", func(t *testing.T, e *env) {
		if e.fx.Advance == nil {
			t.Skip("fixture cannot advance the store clock")
		}
		later, now := e.work(t), e.work(t)
		later.NotBefore = e.fx.Now().Add(time.Minute)
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.EnqueueWork(e.ctx, later); err != nil {
				return err
			}
			return tx.EnqueueWork(e.ctx, now)
		})
		first := e.claimWork(t, "worker-a", 10)
		if len(first) != 1 || first[0].ID != now.ID {
			t.Fatalf("claim before NotBefore = %v, want only %s", workIDs(first), now.ID)
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteWork(e.ctx, first[0]) })
		e.fx.Advance(time.Minute)
		got := e.claimWork(t, "worker-a", 10)
		if len(got) != 1 || got[0].ID != later.ID || !got[0].NotBefore.Equal(later.NotBefore) {
			t.Fatalf("claim at NotBefore = %+v, want %s with its NotBefore", got, later.ID)
		}
	}},
	{"LeaseLifecycle", func(t *testing.T, e *env) {
		res := model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}.Ref()
		var l store.Lease
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			l, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "a", Until: e.fx.Now().Add(time.Minute)})
			return err
		})
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, err := tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "b", Until: e.fx.Now().Add(time.Minute)})
			return err
		}), model.Conflict)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.RenewLease(e.ctx, store.Lease{Resource: res, Fence: l.Fence, Until: e.fx.Now().Add(2 * time.Minute)})
			return err
		})
		stale := l
		stale.Fence.Generation++
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.ReleaseLease(e.ctx, stale) }), model.Conflict)
		e.mustTx(t, func(tx store.Tx) error { return tx.ReleaseLease(e.ctx, l) })
		e.mustTx(t, func(tx store.Tx) error {
			next, err := tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "b", Until: e.fx.Now().Add(time.Minute)})
			if err == nil && next.Fence.Generation <= l.Fence.Generation {
				t.Errorf("generation %d after %d: generations must only grow", next.Fence.Generation, l.Fence.Generation)
			}
			return err
		})
	}},
	{"LeaseReadsTheLiveLease", func(t *testing.T, e *env) {
		res := e.channel.Ref()
		read := func() (store.Lease, error) {
			var l store.Lease
			err := e.viewErr(func(r store.Reader) error {
				var err error
				l, err = r.Lease(e.ctx, res)
				return err
			})
			return l, err
		}
		_, err := read()
		wantCode(t, err, model.NotFound)
		var l store.Lease
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			l, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "host-a", Until: e.fx.Now().Add(time.Minute)})
			return err
		})
		got, err := read()
		if err != nil || got.Resource != res || got.Fence != l.Fence || !got.Until.Equal(l.Until) {
			t.Fatalf("Lease = %+v, %v; want %+v", got, err, l)
		}
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			l, err = tx.RenewLease(e.ctx, store.Lease{Resource: res, Fence: l.Fence, Until: e.fx.Now().Add(2 * time.Minute)})
			return err
		})
		if got, err := read(); err != nil || !got.Until.Equal(l.Until) {
			t.Fatalf("Lease after renewal = %+v, %v; want until %v", got, err, l.Until)
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.ReleaseLease(e.ctx, l) })
		_, err = read()
		wantCode(t, err, model.NotFound)
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.Lease(e.ctx, model.ChannelRef{Tenant: tenantB, ID: e.channel.ID}.Ref())
			return err
		}), model.NotFound)
		if e.fx.Advance == nil {
			return
		}
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "host-b", Until: e.fx.Now().Add(time.Minute)})
			return err
		})
		e.fx.Advance(2 * time.Minute)
		_, err = read()
		wantCode(t, err, model.NotFound)
	}},
	{"OperationUpdateIsFenced", func(t *testing.T, e *env) {
		rec := e.operation(t, "op-fence", []byte{1})
		e.mustTx(t, func(tx store.Tx) error { _, _, err := tx.ClaimOperation(e.ctx, rec); return err })
		res := rec.Operation.Ref.Ref()
		var l store.Lease
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			l, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "worker-a", Until: e.fx.Now().Add(time.Minute)})
			return err
		})
		next := rec.Operation
		next.State, next.Revision = model.ProviderAccepted, "o2"
		stale := l.Fence
		stale.Owner = "worker-b"
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.UpdateOperation(e.ctx, next, "o1", stale)
		}), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.UpdateOperation(e.ctx, next, "o1", store.Fence{})
		}), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.UpdateOperation(e.ctx, next, "o0", l.Fence)
		}), model.Conflict)
		// Late evidence under a stale fence is still recorded.
		e.mustTx(t, func(tx store.Tx) error {
			return tx.AppendAttemptEvidence(e.ctx, store.AttemptEvidence{
				Operation: rec.Operation.Ref, Attempt: e.id(t), Fence: stale, State: model.Delivered,
				ObservedAt: e.fx.Now(),
			})
		})
		e.mustTx(t, func(tx store.Tx) error { return tx.UpdateOperation(e.ctx, next, "o1", l.Fence) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Operation(e.ctx, rec.Operation.Ref)
			if err != nil {
				return err
			}
			if got.Operation.State != model.ProviderAccepted || got.Operation.Revision != "o2" {
				t.Errorf("operation = %+v", got.Operation)
			}
			ev, err := r.Attempts(e.ctx, rec.Operation.Ref, page(10))
			if err == nil && len(ev.Items) != 1 {
				t.Errorf("attempt evidence = %d, want 1", len(ev.Items))
			}
			return err
		})
	}},
	{"OperationByKeyReturnsClaim", func(t *testing.T, e *env) {
		rec := e.operation(t, "op-by-key", []byte{1})
		rec.Command = model.Extension{Name: "conformance.intent", Version: 1, Data: []byte(`{"action":"link"}`)}
		e.mustTx(t, func(tx store.Tx) error {
			if _, _, err := tx.ClaimOperation(e.ctx, rec); err != nil {
				return err
			}
			got, err := tx.OperationByKey(e.ctx, rec.Key)
			// The store clock has not moved: the claim was written now.
			want := rec.Clone()
			want.Operation.UpdatedAt = micro(e.fx.Now())
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Errorf("in-unit OperationByKey = %+v, want the claim %+v", got, want)
			}
			return err
		})
		next := rec.Operation
		next.State, next.Revision = model.Delivered, "o2"
		e.mustTx(t, func(tx store.Tx) error { return tx.UpdateOperation(e.ctx, next, "o1", store.Fence{}) })
		want := rec.Clone()
		want.Operation = next
		want.Operation.UpdatedAt = micro(e.fx.Now())
		e.mustView(t, func(r store.Reader) error {
			got, err := r.OperationByKey(e.ctx, rec.Key)
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Errorf("OperationByKey = %+v, want the current record %+v", got, want)
			}
			return err
		})
	}},
	{"OperationByKeyUnknownIsNotFound", func(t *testing.T, e *env) {
		rec := e.operation(t, "op-known", []byte{1})
		e.mustTx(t, func(tx store.Tx) error { _, _, err := tx.ClaimOperation(e.ctx, rec); return err })
		other := func(change func(*store.OperationKey)) store.OperationKey {
			k := rec.Key
			change(&k)
			return k
		}
		for name, key := range map[string]store.OperationKey{
			"id":     other(func(k *store.OperationKey) { k.ID = "op-unknown" }),
			"caller": other(func(k *store.OperationKey) { k.Caller = "conformance-other" }),
			"kind":   other(func(k *store.OperationKey) { k.Kind = "messages.send" }),
			"target": other(func(k *store.OperationKey) { k.Target = e.channel.Ref() }),
		} {
			err := e.viewErr(func(r store.Reader) error {
				_, err := r.OperationByKey(e.ctx, key)
				return err
			})
			if !model.IsCode(err, model.NotFound) {
				t.Errorf("key with another %s: %v, want not_found", name, err)
			}
		}
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.OperationByKey(e.ctx, other(func(k *store.OperationKey) { k.Kind = "" }))
			return err
		}), model.Validation)
		// A claim rolled back with its unit leaves no key behind.
		rolled := e.operation(t, "op-rolled", []byte{2})
		_ = e.txErr(func(tx store.Tx) error {
			if _, _, err := tx.ClaimOperation(e.ctx, rolled); err != nil {
				return err
			}
			return errAbort
		})
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.OperationByKey(e.ctx, rolled.Key)
			return err
		}), model.NotFound)
	}},
	{"OperationByKeyStaysInTenant", func(t *testing.T, e *env) {
		rec := e.operation(t, "op-tenant", []byte{1})
		e.mustTx(t, func(tx store.Tx) error { _, _, err := tx.ClaimOperation(e.ctx, rec); return err })
		foreign := rec.Key
		foreign.Tenant = tenantB
		foreign.Target = model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}.Ref()
		for _, c := range []struct {
			name string
			key  store.OperationKey
		}{{"a tenant A key", rec.Key}, {"a key naming tenant B", foreign}} {
			wantCode(t, e.s.View(e.ctx, tenantB, func(r store.Reader) error {
				_, err := r.OperationByKey(e.ctx, c.key)
				return err
			}), model.NotFound, "OperationByKey of ", c.name, " read in tenant B")
		}
		// Asked in its own tenant, a key naming another tenant is not found.
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.OperationByKey(e.ctx, foreign)
			return err
		}), model.NotFound)
	}},
}

func (e *env) work(t *testing.T) store.Work {
	t.Helper()
	return store.Work{ID: e.id(t), Kind: workKind, Resource: model.OperationRef{Tenant: tenantA, ID: e.id(t)}.Ref()}
}

func (e *env) claimWork(t *testing.T, owner string, limit int) []store.Work {
	t.Helper()
	var got []store.Work
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		got, err = tx.ClaimWork(e.ctx, store.WorkClaim{
			Kinds: []string{workKind}, Limit: limit, Owner: owner, Until: e.fx.Now().Add(time.Minute),
		})
		return err
	})
	return got
}

func workIDs(ws []store.Work) []model.ID {
	out := make([]model.ID, len(ws))
	for i, w := range ws {
		out[i] = w.ID
	}
	return out
}
