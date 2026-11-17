package conformance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Extension event kind of the desk adapter, delivered only to principals
// holding deskReadPins besides the resource read.
const (
	deskPinned   = "com.example.conformance.desk.pinned"
	deskReadPins = "com.example.conformance.desk.read_pins"
	deskNoPins   = model.ActorID("member-1")
)

// newPinDesk is a desk whose adapter declares deskPinned, registered as an
// event kind authorizing as deskReadPins, which deskNoPins is denied.
func newPinDesk(t *testing.T, factory StoreFactory) *desk {
	t.Helper()
	return newDeskComposed(t, factory, model.Selection{Mode: model.ModePerWorkItem}, func(a *deskAdapter, c *uma.Config) {
		a.events = []string{deskPinned}
		c.Extensions = []adapter.ExtensionDefinition{{
			Name: deskPinned, Version: 1, Kind: model.ExtensionKindEvent,
			RequiredCapability: model.CapabilityReceiveHTTP, AuthorizeAs: deskReadPins,
		}}
		c.Authorizer = policy.AuthorizeFunc(func(_ context.Context, r policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: r.Principal.Actor != deskNoPins || r.Action != deskReadPins}, nil
		})
	})
}

// pinned is an extension.event envelope of deskPinned about the provider
// message msgID, or about the inbox when msgID is empty.
func (d *desk) pinned(event, msgID string, v model.Visibility) model.Envelope {
	return model.Envelope{
		EventID: event, MessageID: msgID, Destination: deskDest, Kind: model.EnvelopeExtensionEvent,
		Audience: model.Audience{Visibility: v}, OccurredAt: time.Unix(1_700_000_100, 0).UTC(),
		Event: &model.Extension{Name: deskPinned, Version: 1, Data: json.RawMessage(`{"message":"` + msgID + `","pin":"` + event + `"}`)},
	}
}

// redact redacts the stored message with provider ID msgID through
// Privacy.Redact.
func (d *desk) redact(msgID string) {
	d.t.Helper()
	var m model.Message
	for _, x := range deskRead(d, store.Reader.Messages, model.Filter{Inboxes: []model.InboxRef{d.inbox}}) {
		for _, r := range x.External {
			if r.Namespace == model.ProviderMessageNamespace && r.ID == msgID {
				m = x
			}
		}
	}
	_, err := d.client.Privacy().Redact(d.ctx, model.Redact{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID("op-redact-" + msgID), ExpectedRevision: m.Revision},
		Target:  m.Ref.Ref(), Message: &m.Ref,
	})
	if err != nil {
		d.t.Fatalf("Redact %s: %v", msgID, err)
	}
}

// delete applies the provider's deletion of the message with provider ID
// msgID: the message stays as a tombstone, unredacted.
func (d *desk) delete(msgID string) {
	d.t.Helper()
	env := d.message("del-"+msgID, msgID, "commenter", time.Unix(1_700_000_050, 0).UTC(), "7")
	env.Kind, env.Content = model.EnvelopeMessageDeleted, model.Content{}
	d.accept(env)
}

// pollPinned opens a subscription to deskPinned for actor from the beginning and
// returns its first batch.
func (d *desk) pollPinned(actor model.ActorID) model.EventBatch {
	d.t.Helper()
	return d.pollPinnedIn(actor, nil)
}

// pollPinnedIn is pollPinned limited to the inboxes, none meaning all.
func (d *desk) pollPinnedIn(actor model.ActorID, inboxes []model.InboxRef) model.EventBatch {
	d.t.Helper()
	op, consumer := "op-sub-"+string(actor), "pins"
	if len(inboxes) > 0 {
		op, consumer = op+"-"+string(inboxes[0].ID), consumer+"-"+string(inboxes[0].ID)
	}
	c, err := d.engine.For(model.Principal{Tenant: serviceTenant, Actor: actor})
	if err != nil {
		d.t.Fatal(err)
	}
	sub, err := c.Events().Open(d.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(op)},
		Consumer: consumer, Filter: model.Filter{Inboxes: inboxes, Kinds: []string{deskPinned}}, Start: model.StartBeginning,
		PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		d.t.Fatalf("Events.Open: %v", err)
	}
	b, err := c.Events().Poll(d.ctx, model.Poll{Call: model.Call{Correlation: "corr"}, Subscription: sub.Subscription.Ref, Limit: 50})
	if err != nil {
		d.t.Fatalf("Events.Poll: %v", err)
	}
	return b
}

// serviceExtensionEvents: registered extension events pass through the
// store's event log intact. A notice about a message is about that message
// under its narrower audience, a notice about the inbox under its own, a
// redelivery appends nothing, a notice about a redacted or a
// provider-deleted message nothing, and an unregistered kind is refused. A subscriber reads the events in
// order with their payloads unchanged; one without the kind's policy
// action reads none.
func serviceExtensionEvents(t *testing.T, factory StoreFactory) {
	d := newPinDesk(t, factory)
	d.accept(d.message("e-1", "c-1", "commenter", time.Unix(1_700_000_000, 0).UTC(), "7"))
	narrowed := d.message("e-2", "c-2", "commenter", time.Unix(1_700_000_000, 0).UTC(), "7")
	narrowed.Audience = model.Audience{Visibility: model.Internal, PolicyRef: "staff"}
	d.accept(narrowed)
	d.accept(d.message("e-3", "c-3", "commenter", time.Unix(1_700_000_000, 0).UTC(), "7"))
	d.redact("c-3")
	d.accept(d.message("e-4", "c-4", "commenter", time.Unix(1_700_000_000, 0).UTC(), "7"))
	d.delete("c-4")

	want := []model.Envelope{
		d.pinned("pin-1", "c-1", model.Internal),
		d.pinned("pin-2", "c-2", model.Public),
		d.pinned("pin-3", "", model.Public),
	}
	for _, env := range want {
		d.accept(env)
	}
	d.accept(want[0])
	d.accept(d.pinned("pin-4", "c-3", model.Public))
	d.accept(d.pinned("pin-6", "c-4", model.Public))
	unknown := d.pinned("pin-5", "", model.Public)
	unknown.Event.Name = "com.example.conformance.desk.unregistered"
	if _, err := d.sink.Accept(d.ctx, model.Ingress{Envelope: unknown}); model.CodeOf(err) != model.Validation {
		t.Fatalf("unregistered event kind = %v, want validation", err)
	}

	msgs := deskRead(d, store.Reader.Messages, model.Filter{Inboxes: []model.InboxRef{d.inbox}})
	byProvider := map[string]model.Message{}
	for _, m := range msgs {
		for _, r := range m.External {
			if r.Namespace == model.ProviderMessageNamespace {
				byProvider[r.ID] = m
			}
		}
	}
	audiences := []model.Audience{{Visibility: model.Internal}, narrowed.Audience, {Visibility: model.Public}}
	resources := []model.Ref{byProvider["c-1"].Ref.Ref(), byProvider["c-2"].Ref.Ref(), d.inbox.Ref()}

	got := d.pollPinned("staff-1").Events
	if len(got) != len(want) {
		t.Fatalf("subscriber got %d pinned events, want %d: one per notice, none for a redelivery or a redacted or deleted message", len(got), len(want))
	}
	for i, ev := range got {
		x := want[i].Event
		switch {
		case ev.Kind != deskPinned || ev.Payload.Name != x.Name || ev.Payload.Version != x.Version || !sameJSON(ev.Payload.Data, x.Data):
			t.Errorf("event %d = %s %s v%d %s, want the emitted %s", i, ev.Kind, ev.Payload.Name, ev.Payload.Version, ev.Payload.Data, x.Data)
		case ev.Resource != resources[i]:
			t.Errorf("event %d resource = %+v, want %+v", i, ev.Resource, resources[i])
		case ev.Audience != audiences[i]:
			t.Errorf("event %d audience = %+v, want %+v", i, ev.Audience, audiences[i])
		}
	}
	if b := d.pollPinned(deskNoPins); len(b.Events) != 0 {
		t.Fatalf("subscriber without %s got %d pinned events, want none", deskReadPins, len(b.Events))
	}
}

// serviceExtensionEventsInboxFilter: a subscription or listing limited to an
// inbox selects the extension events about that inbox as well as the ones
// about its messages, and none about another inbox.
func serviceExtensionEventsInboxFilter(t *testing.T, factory StoreFactory) {
	d := newPinDesk(t, factory)
	d.accept(d.message("e-1", "c-1", "commenter", time.Unix(1_700_000_000, 0).UTC(), "7"))
	d.accept(d.pinned("pin-1", "c-1", model.Public))
	d.accept(d.pinned("pin-2", "", model.Public))
	other := []model.InboxRef{{Tenant: d.inbox.Tenant, ID: "00000000-0000-4000-8000-00000000f00d"}}
	mine := []model.InboxRef{d.inbox}

	resources := func(evs []model.Event) []model.Ref {
		var out []model.Ref
		for _, ev := range evs {
			out = append(out, ev.Resource)
		}
		return out
	}
	check := func(what string, got []model.Event, wantInbox bool) {
		t.Helper()
		switch rs := resources(got); {
		case !wantInbox && len(rs) != 0:
			t.Errorf("%s = %+v, want none: another inbox's events are not selected", what, rs)
		case wantInbox && (len(rs) != 2 || rs[0].Kind != model.KindMessage || rs[1] != d.inbox.Ref()):
			t.Errorf("%s = %+v, want the message notice then the inbox notice", what, rs)
		}
	}
	check("poll in the inbox", d.pollPinnedIn("staff-1", mine).Events, true)
	check("poll in another inbox", d.pollPinnedIn("staff-1", other).Events, false)
	list := func(in []model.InboxRef) []model.Event {
		t.Helper()
		p, err := d.client.Events().List(d.ctx, model.ListEvents{
			Call:   model.Call{Correlation: "corr"},
			Filter: model.Filter{Inboxes: in, Kinds: []string{deskPinned}}, Start: model.StartBeginning, Limit: 50,
		})
		if err != nil {
			t.Fatalf("Events.List: %v", err)
		}
		return p.Items
	}
	check("list in the inbox", list(mine), true)
	check("list in another inbox", list(other), false)
}
