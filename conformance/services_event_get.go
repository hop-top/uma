package conformance

import (
	"reflect"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
)

// eventGetCases read one event through Events.Get: the stored event
// returned by ID; an internal note's event hidden from a principal limited
// to public content, with the answer for an event that does not exist, and
// served to staff; another tenant's principal and a ref naming another
// tenant answered the same way; an event.list denial that reads nothing;
// an event purged by retention answered as not found; and no write unit, no
// subscription row and no event.
var eventGetCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"Returns", eventGetReturns},
	{"HidesInternalNote", eventGetHidesInternalNote},
	{"Foreign", eventGetForeign},
	{"Denied", eventGetDenied},
	{"Purged", eventGetPurged},
	{"ReadOnly", eventGetReadOnly},
}

func eventGetReq(ref model.EventRef) model.Lookup[model.EventRef] {
	return model.Lookup[model.EventRef]{Call: model.Call{Correlation: "get"}, Ref: ref}
}

func (env *serviceEnv) eventRef(ev model.Event) model.EventRef {
	return model.EventRef{Tenant: ev.Tenant, ID: ev.ID}
}

// missingEvent is a ref no event of the tenant ever had.
func (env *serviceEnv) missingEvent() model.EventRef {
	return model.EventRef{Tenant: serviceTenant, ID: env.id()}
}

// noteEvents returns the events about the internal note's message, listed
// as staff.
func (env *serviceEnv) noteEvents(note model.Acceptance, from model.Cursor) []model.Event {
	env.t.Helper()
	env.policy.setStaff(true)
	defer env.policy.setStaff(false)
	var out []model.Event
	for _, ev := range env.listEvents(env.client, eventListReq(model.StartCheckpoint, from, uma.MaxEventListLimit)).Items {
		if ev.Resource == note.Operation.ResultMessage.Ref() {
			out = append(out, ev)
		}
	}
	if len(out) == 0 {
		env.t.Fatal("staff list holds no event of the internal note")
	}
	return out
}

// requireSameNotFound fails unless got is the NotFound error want is: the
// same code and the same fields, so nothing tells the two apart.
func requireSameNotFound(t *testing.T, what string, got, want error) {
	t.Helper()
	serviceCode(t, got, model.NotFound)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s error = %+v, want the one for an event that does not exist, %+v", what, got, want)
	}
}

// eventGetReturns fetches events listed from the log by ID and gets the
// stored event back, whichever the store.
func eventGetReturns(t *testing.T, env *serviceEnv) {
	listed := env.listEvents(env.client, eventListReq(model.StartBeginning, "", uma.MaxEventListLimit))
	evs := env.appendLogEvents(2)
	for _, want := range append(listed.Items, evs...) {
		got, err := env.client.Events().Get(env.ctx, eventGetReq(env.eventRef(want)))
		if err != nil {
			t.Fatalf("Events.Get(%s): %v", want.ID, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Events.Get(%s) = %+v, want %+v", want.ID, got, want)
		}
	}
	_, err := env.client.Events().Get(env.ctx, eventGetReq(env.missingEvent()))
	serviceCode(t, err, model.NotFound)
	_, err = env.client.Events().Get(env.ctx, eventGetReq(model.EventRef{Tenant: serviceTenant}))
	serviceCode(t, err, model.Validation)
}

// eventGetHidesInternalNote is the audience check: the member cannot fetch
// an event about an internal note, whose answer is that of an event that
// does not exist; staff can.
func eventGetHidesInternalNote(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	note := env.note("staff only")
	events := env.noteEvents(note, tail)
	_, missing := env.client.Events().Get(env.ctx, eventGetReq(env.missingEvent()))
	serviceCode(t, missing, model.NotFound)
	for _, ev := range events {
		got, err := env.client.Events().Get(env.ctx, eventGetReq(env.eventRef(ev)))
		if err == nil {
			t.Fatalf("member fetched event %s of the internal note: %+v", ev.ID, got)
		}
		requireSameNotFound(t, "member", err, missing)
	}

	env.policy.setStaff(true)
	defer env.policy.setStaff(false)
	for _, want := range events {
		got, err := env.client.Events().Get(env.ctx, eventGetReq(env.eventRef(want)))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("staff Events.Get(%s) = %+v, %v; want %+v", want.ID, got, err, want)
		}
	}
}

// eventGetForeign: another tenant's principal and a ref naming another
// tenant get the answer for an event that does not exist.
func eventGetForeign(t *testing.T, env *serviceEnv) {
	ev := env.appendLogEvents(1)[0]
	_, missing := env.client.Events().Get(env.ctx, eventGetReq(env.missingEvent()))
	serviceCode(t, missing, model.NotFound)

	other := env.clientOf(env.engine, labelsOtherTenant, "staff-1")
	_, err := other.Events().Get(env.ctx, eventGetReq(model.EventRef{Tenant: labelsOtherTenant, ID: ev.ID}))
	requireSameNotFound(t, "other tenant's principal", err, missing)
	_, err = env.client.Events().Get(env.ctx, eventGetReq(model.EventRef{Tenant: labelsOtherTenant, ID: ev.ID}))
	requireSameNotFound(t, "ref of another tenant", err, missing)
}

// eventGetDenied denies event.list: Get is Unauthorized and opens no store
// unit.
func eventGetDenied(t *testing.T, env *serviceEnv) {
	ev := env.appendLogEvents(1)[0]
	env.policy.deny(model.ActionEventList)
	counting, e := env.countingEngine()
	c := env.clientOf(e, serviceTenant, "staff-1")
	counting.reset()
	_, err := c.Events().Get(env.ctx, eventGetReq(env.eventRef(ev)))
	serviceCode(t, err, model.Unauthorized)
	if v, w := counting.views.Load(), counting.transacts.Load(); v+w != 0 {
		t.Fatalf("denied Get opened %d read and %d write units, want none", v, w)
	}
}

// eventGetPurged purges events past the horizon: a purged event reads as
// not found, as a missing one does, and a retained one is still returned.
func eventGetPurged(t *testing.T, env *serviceEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	old := env.listEvents(env.client, eventListReq(model.StartBeginning, "", 1)).Items
	env.fx.Advance(2 * serviceHorizon)
	env.clock.Advance(2 * serviceHorizon)
	recent := env.appendLogEvents(1)
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "staff-1"}, Call: model.Call{Correlation: "purge"}}
	if rep, err := env.engine.Runtime().Purge(env.ctx, call); err != nil || rep.Events < 2 {
		t.Fatalf("Purge = %+v, %v; want the seeded events purged", rep, err)
	}

	_, missing := env.client.Events().Get(env.ctx, eventGetReq(env.missingEvent()))
	serviceCode(t, missing, model.NotFound)
	_, err := env.client.Events().Get(env.ctx, eventGetReq(env.eventRef(old[0])))
	requireSameNotFound(t, "purged event", err, missing)
	got, err := env.client.Events().Get(env.ctx, eventGetReq(env.eventRef(recent[0])))
	if err != nil || !reflect.DeepEqual(got, recent[0]) {
		t.Fatalf("Events.Get(retained) = %+v, %v; want %+v", got, err, recent[0])
	}
}

// eventGetReadOnly gets through an engine that counts write units: none is
// opened, the event log head does not move and no subscription row appears.
func eventGetReadOnly(t *testing.T, env *serviceEnv) {
	evs := env.appendLogEvents(2)
	counting, e := env.countingEngine()
	c := env.clientOf(e, serviceTenant, "staff-1")
	head := env.eventHead()
	counting.reset()
	for _, ev := range evs {
		if _, err := c.Events().Get(env.ctx, eventGetReq(env.eventRef(ev))); err != nil {
			t.Fatalf("Events.Get(%s): %v", ev.ID, err)
		}
	}
	if n := counting.transacts.Load(); n != 0 {
		t.Fatalf("Get opened %d write units, want none", n)
	}
	if got := env.eventHead(); got != head {
		t.Fatalf("event head moved %q -> %q, want no event", head, got)
	}
	env.requireNoSubscriptionRows()
}
