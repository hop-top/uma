package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// requireNoSubscriptionRows fails when the backend, if it can count them,
// holds a subscription row: no case of the event list group opens one.
func (env *serviceEnv) requireNoSubscriptionRows() {
	env.t.Helper()
	if env.fx.SubscriptionRows == nil {
		return
	}
	n, err := env.fx.SubscriptionRows(env.ctx)
	if err != nil {
		env.t.Fatalf("SubscriptionRows: %v", err)
	}
	if n != 0 {
		env.t.Fatalf("store holds %d subscription rows after Events.List, want none", n)
	}
}

// eventListFilter selects by inbox, by kind and by both. The filter runs in
// the store, before Limit: a page of Limit 1 holds one event the filter
// keeps, however many it drops before it, and the pages together hold
// exactly the kept events and end at the head.
func eventListFilter(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	conv2 := env.conversation(env.inbox2, model.ConversationOpen)
	const received, changed = model.EventMessageReceived, model.EventConversationChanged
	var (
		in1Received = env.appendEventsOf(env.inbox, env.conv, received, 1)
		in2Received = env.appendEventsOf(env.inbox2, conv2, received, 1)
		in1Changed  = env.appendEventsOf(env.inbox, env.conv, changed, 1)
		in2Changed  = env.appendEventsOf(env.inbox2, conv2, changed, 1)
		in1Again    = env.appendEventsOf(env.inbox, env.conv, received, 1)
	)
	head := env.eventHead()
	concat := func(parts ...[]model.Event) []model.Event { return slices.Concat(parts...) }
	for _, c := range []struct {
		name   string
		filter model.Filter
		want   []model.Event
	}{
		{"inbox", model.Filter{Inboxes: []model.InboxRef{env.inbox2}}, concat(in2Received, in2Changed)},
		{"kind", model.Filter{Kinds: []string{received}}, concat(in1Received, in2Received, in1Again)},
		{"inbox and kind", model.Filter{Inboxes: []model.InboxRef{env.inbox}, Kinds: []string{changed}}, in1Changed},
		{"both inboxes", model.Filter{Inboxes: []model.InboxRef{env.inbox, env.inbox2}, Kinds: []string{changed}}, concat(in1Changed, in2Changed)},
	} {
		req := eventListReq(model.StartCheckpoint, tail, 0)
		req.Filter = c.filter
		p := env.listEvents(env.client, req)
		requireEventIDs(t, c.name+" page", p.Items, c.want)
		if !p.Complete || p.Next != head {
			t.Fatalf("%s page = complete %v, next %q; want complete at the head %q", c.name, p.Complete, p.Next, head)
		}

		// One kept event per call: each page holds one, advances, and the
		// last reaches the head.
		var got []model.Event
		at := tail
		for i := range c.want {
			req := eventListReq(model.StartCheckpoint, at, 1)
			req.Filter = c.filter
			page := env.listEvents(env.client, req)
			if len(page.Items) != 1 || page.Next == at || page.Complete != (i == len(c.want)-1) {
				t.Fatalf("%s: page %d = %d items, next %q (from %q), complete %v; want one kept event, complete only on the last",
					c.name, i, len(page.Items), page.Next, at, page.Complete)
			}
			got = append(got, page.Items...)
			at = page.Next
		}
		requireEventIDs(t, c.name+" one-event pages", got, c.want)
		if at != head {
			t.Fatalf("%s: one-event pages ended at %q, want the head %q", c.name, at, head)
		}
	}
}

// eventListFilterRejects refuses filter fields Events.List does not honour,
// an unknown kind and an inbox of another tenant, reading nothing.
func eventListFilterRejects(t *testing.T, env *serviceEnv) {
	since := env.clock.Now()
	for _, c := range []struct {
		name   string
		filter model.Filter
		code   model.ErrorCode
	}{
		{"statuses", model.Filter{Statuses: []string{model.ConversationOpen}}, model.Validation},
		{"text", model.Filter{Text: "hello"}, model.Validation},
		{"before", model.Filter{Before: &since}, model.Validation},
		{"unknown kind", model.Filter{Kinds: []string{"com.example.conformance.unregistered"}}, model.Validation},
		{"foreign inbox", model.Filter{Inboxes: []model.InboxRef{{Tenant: "other-tenant", ID: env.inbox.ID}}}, model.NotFound},
		{"foreign conversation", model.Filter{Conversation: &model.ConversationRef{
			Inbox: model.InboxRef{Tenant: "other-tenant", ID: env.inbox.ID}, ID: env.conv.Ref.ID}}, model.NotFound},
	} {
		req := eventListReq(model.StartBeginning, "", 0)
		req.Filter = c.filter
		_, err := env.client.Events().List(env.ctx, req)
		if model.CodeOf(err) != c.code {
			t.Errorf("filter %s: error = %v, want %s", c.name, err, c.code)
		}
	}
}

// eventListEmptyLog lists a log never written: the tail and the beginning
// are empty and complete, and Next is the empty position, which a
// checkpoint start does not accept. The caller continues from the
// beginning, and the first events are listed from there.
func eventListEmptyLog(t *testing.T, env *serviceEnv) {
	tail := env.listEvents(env.client, eventListReq(model.StartTail, "", 0))
	if len(tail.Items) != 0 || !tail.Complete || tail.Next != "" {
		t.Fatalf("tail of the empty log = %+v, want an empty, complete page at the empty position", tail)
	}
	first := env.listEvents(env.client, eventListReq(model.StartBeginning, "", 0))
	if len(first.Items) != 0 || !first.Complete || first.Next != "" {
		t.Fatalf("beginning of the empty log = %+v, want an empty, complete page at the empty position", first)
	}
	_, err := env.client.Events().List(env.ctx, eventListReq(model.StartCheckpoint, tail.Next, 0))
	serviceCode(t, err, model.Validation)

	evs := env.appendLogEvents(2)
	p := env.listEvents(env.client, eventListReq(model.StartBeginning, "", 0))
	requireEventIDs(t, "first events", p.Items, evs)
	if !p.Complete || p.Next != env.eventHead() {
		t.Fatalf("first page = complete %v, next %q; want complete at the head %q", p.Complete, p.Next, env.eventHead())
	}
	env.requireNoSubscriptionRows()
}

// eventListHidesDeniedExtension lists events of a registered extension
// kind as a principal denied the kind's policy action and as one holding
// it. The denied principal's page omits them, listed by kind or among
// everything else, and ends at the same position as the other's page.
func eventListHidesDeniedExtension(t *testing.T, factory StoreFactory) {
	d := newPinDesk(t, factory)
	at := time.Unix(1_700_000_000, 0).UTC()
	d.accept(d.message("e-1", "c-1", "commenter", at, "7"))
	d.accept(d.pinned("pin-1", "c-1", model.Public))
	d.accept(d.pinned("pin-2", "", model.Public))
	d.accept(d.message("e-2", "c-2", "commenter", at, "7"))

	as := func(actor model.ActorID) uma.Client {
		c, err := d.engine.For(model.Principal{Tenant: serviceTenant, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	list := func(c uma.Client, f model.Filter) model.Page[model.Event] {
		p, err := c.Events().List(d.ctx, model.ListEvents{
			Call: model.Call{Correlation: "list"}, Filter: f, Start: model.StartBeginning, Limit: uma.MaxEventListLimit,
		})
		if err != nil {
			t.Fatalf("Events.List: %v", err)
		}
		return p
	}
	pins := func(p model.Page[model.Event]) int {
		n := 0
		for _, ev := range p.Items {
			if ev.Kind == deskPinned {
				n++
			}
		}
		return n
	}
	byKind := model.Filter{Kinds: []string{deskPinned}}

	holder, denied := as("staff-1"), as(deskNoPins)
	if p := list(holder, byKind); pins(p) != 2 || len(p.Items) != 2 {
		t.Fatalf("holder of %s listed %d events (%d pinned) by kind, want the 2 pinned", deskReadPins, len(p.Items), pins(p))
	}
	if p := list(denied, byKind); len(p.Items) != 0 || !p.Complete {
		t.Fatalf("principal denied %s listed %+v by kind, want an empty, complete page", deskReadPins, p)
	}
	all, rest := list(holder, model.Filter{}), list(denied, model.Filter{})
	if pins(all) != 2 || pins(rest) != 0 || len(rest.Items) != len(all.Items)-2 || len(rest.Items) == 0 {
		t.Fatalf("unfiltered pages hold %d (%d pinned) and %d (%d pinned) events; want the denied page to lack exactly the pinned ones",
			len(all.Items), pins(all), len(rest.Items), pins(rest))
	}
	if rest.Next != all.Next || rest.Complete != all.Complete {
		t.Fatalf("denied page ends at %q (complete %v), holder's at %q (complete %v); want the same position",
			rest.Next, rest.Complete, all.Next, all.Complete)
	}
}
