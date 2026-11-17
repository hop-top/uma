package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// retentionCases cover Purge per record family and the event base it
// records. Every case writes old records, advances the store clock and
// writes recent ones, so a purge that deletes unexpired records fails.
var retentionCases = []storeCase{
	{"EventsPurgeAPrefixAndRecordTheBase", func(t *testing.T, e *env) {
		e.needClock(t)
		old := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two"), e.event(t, "conformance.one")}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, old) })
		var first model.Page[model.Event]
		e.mustView(t, func(r store.Reader) error {
			var err error
			first, err = r.Events(e.ctx, model.Filter{}, "", 1)
			return err
		})
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		recent := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two")}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, recent) })
		head := e.eventHead(t)

		got := e.purge(t, store.FamilyEvents, horizon, 2)
		if got.Purged != 2 || got.Complete {
			t.Fatalf("bounded purge = %+v, want 2 purged and more left", got)
		}
		got = e.purge(t, store.FamilyEvents, horizon, 10)
		if got.Purged != 1 || !got.Complete || got.EventBase == "" {
			t.Fatalf("purge = %+v, want the last old event, complete, with a base", got)
		}
		if again := e.purge(t, store.FamilyEvents, horizon, 10); again.Purged != 0 || !again.Complete || again.EventBase != got.EventBase {
			t.Errorf("repeated purge = %+v, want nothing purged and the same base", again)
		}
		e.mustView(t, func(r store.Reader) error {
			base, err := r.EventBase(e.ctx)
			if err != nil {
				return err
			}
			if base != got.EventBase {
				t.Errorf("EventBase = %q, want the purge's %q", base, got.EventBase)
			}
			if h, err := r.EventHead(e.ctx); err != nil || h != head {
				t.Errorf("head after purge = %q (%v), want %q unchanged", h, err, head)
			}
			for _, c := range []struct {
				name   string
				behind model.Cursor
			}{{"the start of the log", ""}, {"a checkpoint before the base", first.Next}} {
				_, err := r.Events(e.ctx, model.Filter{}, c.behind, 10)
				wantCode(t, err, model.CursorExpired, "Events from ", c.name)
			}
			rest, err := r.Events(e.ctx, model.Filter{}, base, 10)
			if err != nil {
				return err
			}
			if len(rest.Items) != 2 || rest.Items[0].ID != recent[0].ID || rest.Items[1].ID != recent[1].ID || rest.Next != head {
				t.Errorf("retained events = %v next %q, want the recent ones to %q", eventIDs(rest.Items), rest.Next, head)
			}
			return nil
		})
	}},
	{"EventBaseOfAnUnpurgedLogIsTheBeginning", func(t *testing.T, e *env) {
		e.mustView(t, func(r store.Reader) error {
			base, err := r.EventBase(e.ctx)
			if err == nil && base != "" {
				t.Errorf("base of an empty log = %q, want \"\"", base)
			}
			return err
		})
		if got := e.purge(t, store.FamilyEvents, e.fx.Now().Add(time.Hour), 10); got.Purged != 0 || !got.Complete || got.EventBase != "" {
			t.Errorf("purge of an empty log = %+v", got)
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.one")}) })
		if got := e.purge(t, store.FamilyEvents, e.fx.Now().Add(-time.Hour), 10); got.Purged != 0 || !got.Complete || got.EventBase != "" {
			t.Errorf("purge before every event = %+v, want nothing and base \"\"", got)
		}
		if got := e.allEvents(t, model.Filter{}, 10); len(got) != 1 {
			t.Errorf("events from the beginning = %d, want 1", len(got))
		}
	}},
	{"OperationsPurgeOnlyUnreferencedOldRecords", func(t *testing.T, e *env) {
		e.needClock(t)
		done, queued, leased, updated := e.operation(t, "op-done", []byte{1}), e.operation(t, "op-queued", []byte{2}),
			e.operation(t, "op-leased", []byte{3}), e.operation(t, "op-updated", []byte{4})
		work := store.Work{ID: e.id(t), Kind: workKind, Resource: queued.Operation.Ref.Ref()}
		e.mustTx(t, func(tx store.Tx) error {
			for _, rec := range []store.OperationRecord{done, queued, leased, updated} {
				if _, _, err := tx.ClaimOperation(e.ctx, rec); err != nil {
					return err
				}
			}
			if err := tx.AppendAttemptEvidence(e.ctx, store.AttemptEvidence{
				Operation: done.Operation.Ref, Attempt: e.id(t), State: model.Delivered, ObservedAt: e.fx.Now(),
			}); err != nil {
				return err
			}
			return tx.EnqueueWork(e.ctx, work)
		})
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		e.mustTx(t, func(tx store.Tx) error {
			if _, err := tx.AcquireLease(e.ctx, store.LeaseRequest{
				Resource: leased.Operation.Ref.Ref(), Owner: "worker", Until: e.fx.Now().Add(time.Hour),
			}); err != nil {
				return err
			}
			next := updated.Operation
			next.State, next.Revision = model.Delivered, "o2"
			return tx.UpdateOperation(e.ctx, next, "o1", store.Fence{})
		})
		recent := e.operation(t, "op-recent", []byte{5})
		e.mustTx(t, func(tx store.Tx) error { _, _, err := tx.ClaimOperation(e.ctx, recent); return err })

		got := e.purge(t, store.FamilyOperations, horizon, 10)
		if got.Purged != 1 || !got.Complete {
			t.Fatalf("purge = %+v, want only the unreferenced old operation", got)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Operation(e.ctx, done.Operation.Ref)
			wantCode(t, err, model.NotFound)
			_, err = r.Attempts(e.ctx, done.Operation.Ref, page(10))
			wantCode(t, err, model.NotFound)
			for _, kept := range []store.OperationRecord{queued, leased, updated, recent} {
				if _, err := r.Operation(e.ctx, kept.Operation.Ref); err != nil {
					t.Errorf("operation %s: %v, want it retained", kept.Key.ID, err)
				}
			}
			return nil
		})
		// Past the horizon the operation key is free again.
		e.mustTx(t, func(tx store.Tx) error {
			_, created, err := tx.ClaimOperation(e.ctx, e.operationWithKey(t, done.Key, []byte{9}))
			if err == nil && !created {
				t.Error("claim of a purged operation key returned the purged record")
			}
			return err
		})
	}},
	{"AttemptsPurgeOldEvidenceOfIdleOperations", func(t *testing.T, e *env) {
		e.needClock(t)
		idle, busy := e.operation(t, "op-idle", []byte{1}), e.operation(t, "op-busy", []byte{2})
		evidence := func(op store.OperationRecord) store.AttemptEvidence {
			return store.AttemptEvidence{Operation: op.Operation.Ref, Attempt: e.id(t), State: model.Failed, ObservedAt: e.fx.Now()}
		}
		e.mustTx(t, func(tx store.Tx) error {
			for _, rec := range []store.OperationRecord{idle, busy} {
				if _, _, err := tx.ClaimOperation(e.ctx, rec); err != nil {
					return err
				}
				if err := tx.AppendAttemptEvidence(e.ctx, evidence(rec)); err != nil {
					return err
				}
			}
			return tx.EnqueueWork(e.ctx, store.Work{ID: e.id(t), Kind: workKind, Resource: busy.Operation.Ref.Ref()})
		})
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		late := evidence(idle)
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendAttemptEvidence(e.ctx, late) })

		if got := e.purge(t, store.FamilyAttempts, horizon, 10); got.Purged != 1 || !got.Complete {
			t.Fatalf("purge = %+v, want the idle operation's old evidence only", got)
		}
		e.mustView(t, func(r store.Reader) error {
			left, err := r.Attempts(e.ctx, idle.Operation.Ref, page(10))
			if err != nil {
				return err
			}
			if len(left.Items) != 1 || left.Items[0].Attempt != late.Attempt {
				t.Errorf("idle evidence = %d items, want only the recent one", len(left.Items))
			}
			kept, err := r.Attempts(e.ctx, busy.Operation.Ref, page(10))
			if err == nil && len(kept.Items) != 1 {
				t.Errorf("evidence of an operation with work = %d items, want 1 kept", len(kept.Items))
			}
			return err
		})
	}},
	{"IngressPurgeFreesTheDedupKey", func(t *testing.T, e *env) {
		e.needClock(t)
		old := e.ingressRecord(t, "dedup-old")
		e.claimIngress(t, old)
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		recent := e.ingressRecord(t, "dedup-recent")
		e.claimIngress(t, recent)

		if got := e.purge(t, store.FamilyIngress, horizon, 10); got.Purged != 1 || !got.Complete {
			t.Fatalf("purge = %+v, want the old ingress record only", got)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Ingress(e.ctx, old.ID)
			wantCode(t, err, model.NotFound)
			_, err = r.Ingress(e.ctx, recent.ID)
			return err
		})
		if _, created := e.claimIngress(t, e.ingressRecord(t, "dedup-old")); !created {
			t.Error("dedup key of a purged ingress record is still claimed")
		}
		if _, created := e.claimIngress(t, e.ingressRecord(t, "dedup-recent")); created {
			t.Error("dedup key of a retained ingress record was released")
		}
	}},
	{"PurgeRejectsInvalidRequests", func(t *testing.T, e *env) {
		for _, c := range []struct {
			name string
			req  store.PurgeRequest
		}{
			{"unknown family", store.PurgeRequest{Family: "unknown", Before: e.fx.Now(), Limit: 10}},
			{"no limit", store.PurgeRequest{Family: store.FamilyEvents, Before: e.fx.Now()}},
			{"no horizon", store.PurgeRequest{Family: store.FamilyEvents, Limit: 10}},
		} {
			wantCode(t, e.txErr(func(tx store.Tx) error { _, err := tx.Purge(e.ctx, c.req); return err }), model.Validation, "Purge with ", c.name)
		}
	}},
	{"PurgeRollsBackWithItsUnit", func(t *testing.T, e *env) {
		e.needClock(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.one")}) })
		e.fx.Advance(time.Minute)
		_ = e.txErr(func(tx store.Tx) error {
			if _, err := tx.Purge(e.ctx, store.PurgeRequest{Family: store.FamilyEvents, Before: e.fx.Now(), Limit: 10}); err != nil {
				return err
			}
			return errAbort
		})
		if got := e.allEvents(t, model.Filter{}, 10); len(got) != 1 {
			t.Errorf("events after a rolled-back purge = %d, want 1", len(got))
		}
	}},
}

// workCases cover claim counting, quarantine and the in-flight cap.
var workCases = []storeCase{
	{"ClaimsCountUnfinishedClaims", func(t *testing.T, e *env) {
		e.needClock(t)
		w := e.work(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.EnqueueWork(e.ctx, w) })
		first := e.claimWork(t, "worker-a", 1)
		if len(first) != 1 || first[0].Claims != 1 {
			t.Fatalf("first claim = %+v, want Claims 1", first)
		}
		e.fx.Advance(2 * time.Minute)
		second := e.claimWork(t, "worker-b", 1)
		if len(second) != 1 || second[0].Claims != 2 {
			t.Fatalf("reclaim after expiry = %+v, want Claims 2", second)
		}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.EnqueueWork(e.ctx, store.Work{ID: e.id(t), Kind: workKind, Resource: w.Resource, Claims: 1})
		}), model.Validation)
	}},
	{"QuarantineHoldsWorkUntilRequeued", func(t *testing.T, e *env) {
		e.needClock(t)
		w := e.work(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.EnqueueWork(e.ctx, w) })
		got := e.claimWork(t, "worker-a", 1)
		stale := got[0]
		stale.Fence.Generation++
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.QuarantineWork(e.ctx, stale) }), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RequeueWork(e.ctx, w.ID) }), model.Conflict)
		e.mustTx(t, func(tx store.Tx) error { return tx.QuarantineWork(e.ctx, got[0]) })
		e.fx.Advance(2 * time.Minute)
		if again := e.claimWork(t, "worker-b", 10); len(again) != 0 {
			t.Fatalf("claimed quarantined work %v", workIDs(again))
		}
		e.mustView(t, func(r store.Reader) error {
			d, err := r.WorkDepth(e.ctx, workKind)
			if err != nil {
				return err
			}
			if d != (store.WorkDepth{Quarantined: 1}) {
				t.Errorf("depth = %+v, want one quarantined item", d)
			}
			q, err := r.QuarantinedWork(e.ctx, w.Resource)
			if err != nil {
				return err
			}
			if len(q) != 1 || q[0].ID != w.ID || q[0].Claims != 1 {
				t.Errorf("quarantined = %+v, want %s with its claim count", q, w.ID)
			}
			return nil
		})
		// The resource lease was released: the resource can be leased at once.
		e.mustTx(t, func(tx store.Tx) error {
			l, err := tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: w.Resource, Owner: "admin", Until: e.fx.Now().Add(time.Minute)})
			if err != nil {
				return err
			}
			return tx.ReleaseLease(e.ctx, l)
		})
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RequeueWork(e.ctx, e.id(t)) }), model.NotFound)
		e.mustTx(t, func(tx store.Tx) error { return tx.RequeueWork(e.ctx, w.ID) })
		back := e.claimWork(t, "worker-b", 10)
		if len(back) != 1 || back[0].ID != w.ID || back[0].Claims != 1 {
			t.Fatalf("claim after requeue = %+v, want %s with Claims reset to 1", back, w.ID)
		}
	}},
	{"InFlightCapBoundsClaimsPerKind", func(t *testing.T, e *env) {
		e.needClock(t)
		const other = "conformance.other"
		a := []store.Work{e.work(t), e.work(t), e.work(t)}
		b := e.work(t)
		b.Kind = other
		e.mustTx(t, func(tx store.Tx) error {
			for _, w := range append(a, b) {
				if err := tx.EnqueueWork(e.ctx, w); err != nil {
					return err
				}
			}
			return nil
		})
		capped := func(owner string, kinds ...string) ([]store.Work, error) {
			var got []store.Work
			err := e.txErr(func(tx store.Tx) error {
				var err error
				got, err = tx.ClaimWork(e.ctx, store.WorkClaim{
					Kinds: kinds, Limit: 10, Owner: owner, Until: e.fx.Now().Add(time.Minute), MaxInFlight: 2,
				})
				return err
			})
			return got, err
		}
		got, err := capped("worker-a", workKind, other)
		if err != nil || len(got) != 3 || got[0].ID != a[0].ID || got[1].ID != a[1].ID || got[2].ID != b.ID {
			t.Fatalf("capped claim = %v (%v), want two of the capped kind and the other kind's item", workIDs(got), err)
		}
		_, err = capped("worker-b", workKind, other)
		wantCode(t, err, model.Unavailable)
		e.mustView(t, func(r store.Reader) error {
			d, err := r.WorkDepth(e.ctx, workKind)
			if err == nil && d != (store.WorkDepth{Pending: 1, Claimed: 2}) {
				t.Errorf("depth at the cap = %+v, want 1 pending and 2 claimed", d)
			}
			return err
		})
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteWork(e.ctx, got[0]) })
		next, err := capped("worker-b", workKind)
		if err != nil || len(next) != 1 || next[0].ID != a[2].ID {
			t.Fatalf("claim after a completion = %v (%v), want the held-back item", workIDs(next), err)
		}
		// At the cap with nothing held back, an empty claim is not saturation.
		if none, err := capped("worker-c", other); err != nil || len(none) != 0 {
			t.Fatalf("claim with nothing pending = %v (%v), want empty and no error", workIDs(none), err)
		}
		// Expired claims do not count against the cap.
		e.fx.Advance(2 * time.Minute)
		e.mustView(t, func(r store.Reader) error {
			d, err := r.WorkDepth(e.ctx, workKind)
			if err == nil && d != (store.WorkDepth{Pending: 2}) {
				t.Errorf("depth after expiry = %+v, want 2 pending", d)
			}
			return err
		})
		if again, err := capped("worker-d", workKind); err != nil || len(again) != 2 {
			t.Fatalf("claim after expiry = %v (%v), want both expired items", workIDs(again), err)
		}
	}},
	{"WorkDepthCountsDeferredAsPending", func(t *testing.T, e *env) {
		deferred := e.work(t)
		deferred.NotBefore = e.fx.Now().Add(time.Hour)
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.EnqueueWork(e.ctx, deferred); err != nil {
				return err
			}
			return tx.EnqueueWork(e.ctx, e.work(t))
		})
		e.claimWork(t, "worker-a", 10)
		e.mustView(t, func(r store.Reader) error {
			d, err := r.WorkDepth(e.ctx, workKind)
			if err == nil && d != (store.WorkDepth{Pending: 1, Claimed: 1}) {
				t.Errorf("depth = %+v, want the deferred item pending and one claimed", d)
			}
			if err != nil {
				return err
			}
			d, err = r.WorkDepth(e.ctx, "conformance.none")
			if err == nil && d != (store.WorkDepth{}) {
				t.Errorf("depth of an unused kind = %+v", d)
			}
			return err
		})
	}},
}

func (e *env) needClock(t *testing.T) {
	t.Helper()
	if e.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
}

// purge runs one Purge call in its own unit.
func (e *env) purge(t *testing.T, family string, before time.Time, limit int) store.PurgeResult {
	t.Helper()
	var got store.PurgeResult
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		got, err = tx.Purge(e.ctx, store.PurgeRequest{Family: family, Before: before, Limit: limit})
		return err
	})
	return got
}

func (e *env) eventHead(t *testing.T) model.Cursor {
	t.Helper()
	var head model.Cursor
	e.mustView(t, func(r store.Reader) error {
		var err error
		head, err = r.EventHead(e.ctx)
		return err
	})
	return head
}

// operationWithKey is a new operation record claiming key.
func (e *env) operationWithKey(t *testing.T, key store.OperationKey, fingerprint []byte) store.OperationRecord {
	t.Helper()
	rec := e.operation(t, key.ID, fingerprint)
	rec.Key = key
	return rec
}

// retentionAndQuarantine checks that an event base, a purged operation key,
// claim counts and a quarantine persist across cross.
func retentionAndQuarantine(t *testing.T, e *env, cross func(*testing.T)) {
	e.needClock(t)
	e.mustTx(t, func(tx store.Tx) error {
		return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two")})
	})
	poisoned, counted := e.work(t), e.work(t)
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.EnqueueWork(e.ctx, poisoned); err != nil {
			return err
		}
		return tx.EnqueueWork(e.ctx, counted)
	})
	claimed := e.claimWork(t, "worker-a", 2)
	e.mustTx(t, func(tx store.Tx) error { return tx.QuarantineWork(e.ctx, claimed[0]) })
	e.fx.Advance(2 * time.Minute)
	base := e.purge(t, store.FamilyEvents, e.fx.Now(), 10).EventBase

	cross(t)

	e.mustView(t, func(r store.Reader) error {
		got, err := r.EventBase(e.ctx)
		if err != nil {
			return err
		}
		if got != base || got == "" {
			t.Errorf("event base = %q, want %q kept", got, base)
		}
		_, err = r.Events(e.ctx, model.Filter{}, "", 10)
		wantCode(t, err, model.CursorExpired)
		q, err := r.QuarantinedWork(e.ctx, poisoned.Resource)
		if err == nil && (len(q) != 1 || q[0].ID != poisoned.ID) {
			t.Errorf("quarantined = %v, want %s kept", workIDs(q), poisoned.ID)
		}
		return err
	})
	again := e.claimWork(t, "worker-b", 10)
	if len(again) != 1 || again[0].ID != counted.ID || again[0].Claims != 2 {
		t.Fatalf("reclaim = %+v, want %s with Claims 2", again, counted.ID)
	}
}
