package events_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

func (h *evHarness) eventRef(ev model.Event) model.EventRef {
	return model.EventRef{Tenant: ev.Tenant, ID: ev.ID}
}

func getReq(ref model.EventRef) model.Lookup[model.EventRef] {
	return model.Lookup[model.EventRef]{Call: model.Call{Correlation: "get"}, Ref: ref}
}

// internalBlind denies every read of internal content to the actor it names,
// as a policy limiting an external-facing actor to public content does.
type internalBlind struct {
	policy.Authorizer
	actor model.ActorID
}

func (b internalBlind) Authorize(ctx context.Context, a policy.Authorization) (policy.Decision, error) {
	if a.Principal.Actor == b.actor && a.Audience.Visibility == model.Internal {
		return policy.Decision{}, nil
	}
	return b.Authorizer.Authorize(ctx, a)
}

func TestGetReturnsTheEvent(t *testing.T) {
	h := newEvHarness(t, nil)
	h.message(enginetest.InboxOne)
	want := h.message(enginetest.InboxOne)
	h.message(enginetest.InboxOne)

	got, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(want)))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, want the stored event %+v", got, want)
	}
	got.Payload.Data[0] ^= 0xff
	again, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(want)))
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("second Get = %+v, %v: the first result aliases stored state", again, err)
	}
}

func TestGetValidatesTheRequest(t *testing.T) {
	h := newEvHarness(t, nil)
	for name, req := range map[string]model.Lookup[model.EventRef]{
		"no ID":          getReq(model.EventRef{Tenant: enginetest.TenantA}),
		"no tenant":      getReq(model.EventRef{ID: seqID(0xe0, 1)}),
		"no correlation": {Ref: model.EventRef{Tenant: enginetest.TenantA, ID: seqID(0xe0, 1)}},
	} {
		_, err := h.bot.Events().Get(h.ctx, req)
		if !model.IsCode(err, model.Validation) {
			t.Errorf("%s: err = %v, want Validation", name, err)
		}
	}
}

func TestGetCallDenied(t *testing.T) {
	h, cs := newCountingHarness(t)
	ev := h.message(enginetest.InboxOne)
	if _, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(ev))); err != nil {
		t.Fatal(err)
	}
	var call *policy.Authorization
	for i, a := range h.gate.requests {
		if a.Action == model.ActionEventList {
			call = &h.gate.requests[i]
		}
	}
	if call == nil || call.Resource != model.EventLogRef(enginetest.TenantA) || call.Principal.Actor != botActor {
		t.Fatalf("event.list authorization = %+v, want the bot on the tenant event log", call)
	}

	h.gate.denyAction(model.ActionEventList)
	cs.reset()
	_, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(ev)))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	if v, w := cs.views.Load(), cs.transacts.Load(); v+w != 0 {
		t.Fatalf("denied Get opened %d read and %d write units, want none", v, w)
	}
}

// An event the principal may not read, one that never existed, one in
// another tenant and one purged by retention are the same answer.
func TestGetUnreadableMissingAndPurgedAreIndistinguishable(t *testing.T) {
	h := newEvHarness(t, nil)
	purged := h.message(enginetest.InboxOne)
	hidden := h.message(enginetest.InboxOne)
	otherInbox := h.message(enginetest.InboxTwo)
	h.gate.deny(hidden.Resource)
	h.gate.deny(h.inbox(enginetest.InboxTwo).Ref())
	if err := h.mem.Transact(h.ctx, enginetest.TenantA, func(tx store.Tx) error {
		_, err := tx.Purge(h.ctx, store.PurgeRequest{Family: store.FamilyEvents, Before: evT0.Add(1), Limit: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Purge drops the log's prefix: Limit 1 removes purged, the first event.
	refs := map[string]model.EventRef{
		"unreadable resource": h.eventRef(hidden),
		"unreadable inbox":    h.eventRef(otherInbox),
		"purged":              h.eventRef(purged),
		"never existed":       {Tenant: enginetest.TenantA, ID: seqID(0xee, 99)},
		"other tenant":        {Tenant: enginetest.TenantB, ID: hidden.ID},
	}
	var want *model.Error
	for name, ref := range refs {
		_, err := h.bot.Events().Get(h.ctx, getReq(ref))
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.NotFound {
			t.Fatalf("%s: err = %v, want NotFound", name, err)
		}
		if want == nil {
			want = me
		} else if !reflect.DeepEqual(me, want) {
			t.Errorf("%s: error %+v differs from %+v", name, me, want)
		}
	}
}

func TestGetPolicyFailureIsNotNotFound(t *testing.T) {
	h := newEvHarness(t, nil)
	ev := h.message(enginetest.InboxOne)
	h.gate.failWith(errors.New("policy down"))
	_, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(ev)))
	enginetest.RequireCode(t, err, model.Unavailable, "")
}

// The audience check Poll and List apply holds for Get: an actor limited
// to public content cannot fetch an internal note's event, and the answer
// is the one for an event that does not exist.
func TestGetHidesInternalAudienceEvents(t *testing.T) {
	h := newEvHarness(t, nil, func(c *engine.Config) { c.Authorizer = internalBlind{c.Authorizer, otherActor} })
	public := h.message(enginetest.InboxOne)
	note := h.newMessage(enginetest.InboxOne)
	note.Audience = model.Audience{Visibility: model.Internal}
	h.append(note)
	external := h.client(otherActor)

	if _, err := external.Events().Get(h.ctx, getReq(h.eventRef(public))); err != nil {
		t.Fatalf("external Get of a public event: %v", err)
	}
	if _, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(note))); err != nil {
		t.Fatalf("internal actor Get of an internal event: %v", err)
	}
	_, hiddenErr := external.Events().Get(h.ctx, getReq(h.eventRef(note)))
	_, missingErr := external.Events().Get(h.ctx, getReq(model.EventRef{Tenant: enginetest.TenantA, ID: seqID(0xee, 99)}))
	enginetest.RequireCode(t, hiddenErr, model.NotFound, "")
	if !reflect.DeepEqual(hiddenErr, missingErr) {
		t.Fatalf("hidden = %+v, missing = %+v: want the same error", hiddenErr, missingErr)
	}
}

func TestGetRefusesClosedEngine(t *testing.T) {
	h := newEvHarness(t, nil)
	ev := h.message(enginetest.InboxOne)
	if err := h.e.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, err := h.bot.Events().Get(h.ctx, getReq(h.eventRef(ev)))
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
}
