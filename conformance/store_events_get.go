package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// eventGetStoreCases cover Reader.Event: an event is read back by ID as
// appended, whatever the log's other events; an ID the log never held, an
// ID of another tenant and an event only a rolled-back unit appended are
// NotFound; a unit sees its own appends; and a purged event is NotFound
// while the retained ones are not.
var eventGetStoreCases = []storeCase{
	{"EventReadsBackByID", func(t *testing.T, e *env) {
		evs := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two"), e.event(t, "conformance.one")}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, evs) })
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.lost")}); err != nil {
				return err
			}
			return errAbort
		})
		lost := e.id(t)
		e.mustView(t, func(r store.Reader) error {
			for _, want := range evs {
				got, err := r.Event(e.ctx, model.EventRef{Tenant: tenantA, ID: want.ID})
				if err != nil {
					t.Errorf("Event(%s): %v", want.ID, err)
				} else if !reflect.DeepEqual(got, want) {
					wantEqual(t, "Event("+string(want.ID)+")", got, want)
				}
			}
			_, err := r.Event(e.ctx, model.EventRef{Tenant: tenantA, ID: lost})
			wantCode(t, err, model.NotFound)
			_, err = r.Event(e.ctx, model.EventRef{Tenant: tenantB, ID: evs[0].ID})
			wantCode(t, err, model.NotFound)
			_, err = r.Event(e.ctx, model.EventRef{Tenant: tenantA})
			wantCode(t, err, model.Validation)
			return nil
		})
		var other model.Event
		e.mustTx(t, func(tx store.Tx) error {
			other = e.event(t, "conformance.three")
			if err := tx.AppendEvents(e.ctx, []model.Event{other}); err != nil {
				return err
			}
			got, err := tx.Event(e.ctx, model.EventRef{Tenant: tenantA, ID: other.ID})
			if err != nil || !reflect.DeepEqual(got, other) {
				t.Errorf("Event of the unit's own append = %+v, %v; want %+v", got, err, other)
			}
			return nil
		})
	}},
	{"EventPurgedIsNotFound", func(t *testing.T, e *env) {
		e.needClock(t)
		old := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two")}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, old) })
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		recent := []model.Event{e.event(t, "conformance.one")}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, recent) })
		if got := e.purge(t, store.FamilyEvents, horizon, 10); got.Purged != 2 {
			t.Fatalf("purge = %+v, want the two old events", got)
		}
		e.mustView(t, func(r store.Reader) error {
			for _, ev := range old {
				_, err := r.Event(e.ctx, model.EventRef{Tenant: tenantA, ID: ev.ID})
				wantCode(t, err, model.NotFound, "purged Event(", ev.ID, ")")
			}
			got, err := r.Event(e.ctx, model.EventRef{Tenant: tenantA, ID: recent[0].ID})
			if err != nil || !reflect.DeepEqual(got, recent[0]) {
				t.Errorf("Event(retained) = %+v, %v; want %+v", got, err, recent[0])
			}
			return nil
		})
	}},
}
