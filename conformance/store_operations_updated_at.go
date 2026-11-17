package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// operationUpdatedAtCases pin Operation.UpdatedAt on every read of the
// ledger: the store's write time to the microsecond, the one the list order
// and window use, moved forward by each write and never taken from the
// caller.
var operationUpdatedAtCases = []storeCase{
	{"IsTheWriteTimeOnEveryRead", func(t *testing.T, e *env) {
		// A clock that is not on a microsecond: the time shown is the one the
		// window compares.
		e.advance(t, 1234*time.Nanosecond)
		want := micro(e.fx.Now())
		claimed := e.putOperation(t, opSpec{target: e.inbox.Ref()})
		e.tick(t)
		ref, key := claimed.Operation.Ref, claimed.Key
		reads := map[string]func(store.Reader) (store.OperationRecord, error){
			"Operation": func(r store.Reader) (store.OperationRecord, error) { return r.Operation(e.ctx, ref) },
			"OperationByKey": func(r store.Reader) (store.OperationRecord, error) {
				return r.OperationByKey(e.ctx, key)
			},
			"Operations": func(r store.Reader) (store.OperationRecord, error) {
				p, err := r.Operations(e.ctx, list(10))
				if err != nil || len(p.Items) != 1 {
					t.Fatalf("Operations = %d items, %v, want the one operation", len(p.Items), err)
				}
				return p.Items[0], nil
			},
		}
		for name, read := range reads {
			e.mustView(t, func(r store.Reader) error {
				rec, err := read(r)
				if err != nil {
					return err
				}
				if got := rec.Operation.UpdatedAt; !got.Equal(want) || got.Location() != time.UTC {
					t.Errorf("%s: UpdatedAt = %v, want the write time %v", name, got, want)
				}
				return nil
			})
		}
		// Inside the unit that wrote it, the claim and a replay of the claim
		// report the same time as a later read.
		e.mustTx(t, func(tx store.Tx) error {
			rec, created, err := tx.ClaimOperation(e.ctx, claimed)
			if err != nil || created {
				t.Fatalf("replayed claim = created %v, %v, want the stored operation", created, err)
			}
			if !rec.Operation.UpdatedAt.Equal(want) {
				t.Errorf("replayed claim: UpdatedAt = %v, want %v", rec.Operation.UpdatedAt, want)
			}
			got, err := tx.Operation(e.ctx, ref)
			if err == nil && !got.Operation.UpdatedAt.Equal(want) {
				t.Errorf("Operation in a unit: UpdatedAt = %v, want %v", got.Operation.UpdatedAt, want)
			}
			return err
		})
	}},
	{"ClaimReturnsTheWriteTime", func(t *testing.T, e *env) {
		e.advance(t, 789*time.Nanosecond)
		rec := e.operation(t, "updated-at-claim", []byte{4})
		var got store.OperationRecord
		e.mustTx(t, func(tx store.Tx) error {
			var created bool
			var err error
			got, created, err = tx.ClaimOperation(e.ctx, rec)
			if err == nil && !created {
				t.Fatal("claim did not create the operation")
			}
			return err
		})
		if want := micro(e.fx.Now()); !got.Operation.UpdatedAt.Equal(want) {
			t.Errorf("claim returned UpdatedAt = %v, want the write time %v", got.Operation.UpdatedAt, want)
		}
		stored := e.operationRecord(t, rec.Operation.Ref)
		if !stored.Operation.UpdatedAt.Equal(got.Operation.UpdatedAt) {
			t.Errorf("stored UpdatedAt = %v, claim returned %v", stored.Operation.UpdatedAt, got.Operation.UpdatedAt)
		}
	}},
	{"EveryWriteMovesItForward", func(t *testing.T, e *env) {
		rec := e.putOperation(t, opSpec{target: e.inbox.Ref(), state: model.Queued})
		prev := e.operationRecord(t, rec.Operation.Ref).Operation.UpdatedAt
		for i, state := range []model.DeliveryState{model.Unknown, model.Unknown, model.Delivered} {
			e.advance(t, time.Second+17*time.Nanosecond)
			e.moveOperation(t, &rec, state)
			got := e.operationRecord(t, rec.Operation.Ref).Operation.UpdatedAt
			if want := micro(e.fx.Now()); !got.Equal(want) {
				t.Errorf("write %d: UpdatedAt = %v, want the write time %v", i, got, want)
			}
			if !got.After(prev) {
				t.Errorf("write %d: UpdatedAt = %v, want it after the previous %v", i, got, prev)
			}
			prev = got
		}
		// A read changes nothing.
		e.tick(t)
		if got := e.operationRecord(t, rec.Operation.Ref).Operation.UpdatedAt; !got.Equal(prev) {
			t.Errorf("UpdatedAt after a read = %v, want %v", got, prev)
		}
	}},
	{"IsNotTheCallers", func(t *testing.T, e *env) {
		lie := time.Unix(1_000, 0).UTC()
		rec := e.operation(t, "updated-at-lie", []byte{5})
		rec.Operation.UpdatedAt = lie
		e.mustTx(t, func(tx store.Tx) error {
			got, _, err := tx.ClaimOperation(e.ctx, rec)
			if err == nil && !got.Operation.UpdatedAt.Equal(micro(e.fx.Now())) {
				t.Errorf("claim returned UpdatedAt = %v, want the store's write time %v", got.Operation.UpdatedAt, micro(e.fx.Now()))
			}
			return err
		})
		if got := e.operationRecord(t, rec.Operation.Ref).Operation.UpdatedAt; !got.Equal(micro(e.fx.Now())) {
			t.Errorf("after a claim that supplied %v: UpdatedAt = %v, want the write time %v", lie, got, micro(e.fx.Now()))
		}
		e.tick(t)
		next := rec.Operation
		next.State, next.Revision, next.UpdatedAt = model.Delivered, "r-lie", time.Unix(4_000_000_000, 0).UTC()
		e.mustTx(t, func(tx store.Tx) error {
			return tx.UpdateOperation(e.ctx, next, rec.Operation.Revision, store.Fence{})
		})
		if got := e.operationRecord(t, rec.Operation.Ref).Operation.UpdatedAt; !got.Equal(micro(e.fx.Now())) {
			t.Errorf("after an update that supplied %v: UpdatedAt = %v, want the write time %v", next.UpdatedAt, got, micro(e.fx.Now()))
		}
		// The list reads the same time the window does: the supplied one
		// puts the operation in no window of its own.
		since := lie
		before := lie.Add(time.Hour)
		if got := e.listOperations(t, model.Filter{Since: &since, Before: &before}, 10); len(got) != 0 {
			t.Errorf("window around the supplied time lists %v, want none", got)
		}
	}},
	{"MatchesTheListWindowEdges", func(t *testing.T, e *env) {
		var recs []store.OperationRecord
		for range 3 {
			e.advance(t, time.Second+321*time.Nanosecond)
			recs = append(recs, e.putOperation(t, opSpec{target: e.inbox.Ref()}))
		}
		e.advance(t, time.Second)
		e.moveOperation(t, &recs[0], model.Delivered)
		for _, r := range recs {
			at := e.operationRecord(t, r.Operation.Ref).Operation.UpdatedAt
			only := func(f model.Filter) []model.ID { return e.listOperations(t, f, 100) }
			in := func(f model.Filter, what string) {
				t.Helper()
				if got := only(f); !slices.Contains(got, r.Operation.Ref.ID) {
					t.Errorf("%s: window lists %v, want %s shown at %v", what, got, r.Operation.Ref.ID, at)
				}
			}
			out := func(f model.Filter, what string) {
				t.Helper()
				if slices.Contains(only(f), r.Operation.Ref.ID) {
					t.Errorf("%s: window lists %s shown at %v, want it left out", what, r.Operation.Ref.ID, at)
				}
			}
			past, ahead := at.Add(-time.Microsecond), at.Add(time.Microsecond)
			in(model.Filter{Since: &at}, "Since its UpdatedAt is inclusive")
			out(model.Filter{Since: &ahead}, "Since a microsecond after")
			in(model.Filter{Since: &past}, "Since a microsecond before")
			out(model.Filter{Before: &at}, "Before its UpdatedAt is exclusive")
			in(model.Filter{Before: &ahead}, "Before a microsecond after")
			in(model.Filter{Since: &at, Before: &ahead}, "the one microsecond from UpdatedAt")
		}
	}},
}

// micro is t to the microsecond, in UTC: the precision of the write time
// a store keeps.
func micro(t time.Time) time.Time { return time.UnixMicro(t.UnixMicro()).UTC() }

// operationRecord reads the stored operation ref names.
func (e *env) operationRecord(t *testing.T, ref model.OperationRef) store.OperationRecord {
	t.Helper()
	var rec store.OperationRecord
	e.mustView(t, func(r store.Reader) error {
		var err error
		rec, err = r.Operation(e.ctx, ref)
		return err
	})
	return rec
}

// advance moves the store clock by d, or skips the case when the fixture
// cannot.
func (e *env) advance(t *testing.T, d time.Duration) {
	t.Helper()
	if e.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	e.fx.Advance(d)
}
