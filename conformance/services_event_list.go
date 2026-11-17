package conformance

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// eventListCases read the event log through Events.List: the beginning,
// tail and checkpoint starts; a list handed off to a subscription at its
// Next without gap or duplicate; the Inboxes and Kinds filter, scanning
// before it selects, the Conversation and Since filters, and the fields
// it refuses; events the principal may
// not read hidden without totals while Next still passes them; an
// event.list denial that reads nothing; no write unit, no subscription
// row and no event; a position behind retention expired with List's own
// recovery; and the scan limit's default and cap. The log never written
// and extension events a principal's policy action denies run on their own
// environments (see eventListEmptyLog and eventListHidesDeniedExtension).
var eventListCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"Starts", eventListStarts},
	{"HandoffToSubscription", eventListHandoff},
	{"Filter", eventListFilter},
	{"FilterConversationSince", eventListConversationSince},
	{"FilterRejectsUnsupported", eventListFilterRejects},
	{"HidesUnreadable", eventListHidesUnreadable},
	{"Denied", eventListDenied},
	{"ReadOnly", eventListReadOnly},
	{"CursorExpired", eventListCursorExpired},
	{"LimitCap", eventListLimitCap},
}

// newEventListEnv is a service env whose log holds two committed events,
// so the head is a position a checkpoint start accepts: the tail of a
// log never written is the empty position, continued from the beginning.
func newEventListEnv(t *testing.T, factory StoreFactory) *serviceEnv {
	t.Helper()
	env := newServiceEnv(t, factory)
	env.appendLogEvents(2)
	return env
}

func eventListReq(start string, after model.Cursor, limit int) model.ListEvents {
	return model.ListEvents{Call: model.Call{Correlation: "list"}, Start: start, After: after, Limit: limit}
}

func (env *serviceEnv) listEvents(c uma.Client, req model.ListEvents) model.Page[model.Event] {
	env.t.Helper()
	p, err := c.Events().List(env.ctx, req)
	if err != nil {
		env.t.Fatalf("Events.List(%s, %q, %d): %v", req.Start, req.After, req.Limit, err)
	}
	return p
}

// tailPosition lists from the tail: an empty, complete page at the head.
func (env *serviceEnv) tailPosition() model.Cursor {
	env.t.Helper()
	p := env.listEvents(env.client, eventListReq(model.StartTail, "", 0))
	if len(p.Items) != 0 || !p.Complete || p.Next != env.eventHead() {
		env.t.Fatalf("tail page = %+v, want empty and complete at the head %q", p, env.eventHead())
	}
	return p.Next
}

// appendLogEvents commits n public conversation events in env.inbox in one
// unit and returns them in commit order.
func (env *serviceEnv) appendLogEvents(n int) []model.Event {
	env.t.Helper()
	return env.appendEventsOf(env.inbox, env.conv, "com.example.conformance.listed", n)
}

// appendEventsOf commits n public events of kind about conv, which lives
// in inbox, in one unit and returns them in commit order.
func (env *serviceEnv) appendEventsOf(inbox model.InboxRef, conv model.Conversation, kind string, n int) []model.Event {
	env.t.Helper()
	at := env.clock.Now().UTC()
	evs := make([]model.Event, n)
	for i := range evs {
		evs[i] = model.Event{
			Version: model.EventEnvelopeVersion, ID: env.id(), Kind: kind,
			Tenant: serviceTenant, Inbox: inbox.ID, Resource: conv.Ref.Ref(), Actor: "conformance-actor",
			OccurredAt: at, RecordedAt: at, Audience: model.Audience{Visibility: model.Public},
		}
	}
	env.put(func(tx store.Tx) error { return tx.AppendEvents(env.ctx, evs) })
	return evs
}

func eventIDsOf(evs []model.Event) []model.ID {
	out := make([]model.ID, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}

func requireEventIDs(t *testing.T, what string, got, want []model.Event) {
	t.Helper()
	if g, w := eventIDsOf(got), eventIDsOf(want); !slices.Equal(g, w) {
		t.Fatalf("%s = %v, want %v", what, g, w)
	}
}

// eventListStarts lists from the beginning, from the tail and from a
// checkpoint, paging in commit order.
func eventListStarts(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	evs := env.appendLogEvents(3)

	all := env.listEvents(env.client, eventListReq(model.StartBeginning, "", uma.MaxEventListLimit))
	if !all.Complete || all.Next != env.eventHead() || len(all.Items) < len(evs) {
		t.Fatalf("beginning page = %d items, complete %v, next %q; want every event up to the head %q",
			len(all.Items), all.Complete, all.Next, env.eventHead())
	}
	requireEventIDs(t, "beginning page tail", all.Items[len(all.Items)-len(evs):], evs)

	first := env.listEvents(env.client, eventListReq(model.StartCheckpoint, tail, 2))
	requireEventIDs(t, "first checkpoint page", first.Items, evs[:2])
	if first.Complete {
		t.Fatal("partial page reported complete")
	}
	rest := env.listEvents(env.client, eventListReq(model.StartCheckpoint, first.Next, 2))
	requireEventIDs(t, "second checkpoint page", rest.Items, evs[2:])
	if !rest.Complete || rest.Next != all.Next {
		t.Fatalf("second page = complete %v, next %q; want complete at %q", rest.Complete, rest.Next, all.Next)
	}
	if got := env.tailPosition(); got != all.Next {
		t.Fatalf("tail = %q, want the head %q", got, all.Next)
	}
}

// eventListHandoff lists up to Next, then opens a subscription there: it
// delivers exactly the events committed after the list.
func eventListHandoff(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	listed := env.appendLogEvents(2)
	p := env.listEvents(env.client, eventListReq(model.StartCheckpoint, tail, 0))
	requireEventIDs(t, "listed", p.Items, listed)
	between := env.appendLogEvents(1)
	res, err := env.client.Events().Open(env.ctx, model.OpenSubscription{
		Command: env.cmd(""), Consumer: "after-list", Start: model.StartCheckpoint, After: p.Next,
		PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		t.Fatalf("Open at the list's Next: %v", err)
	}
	after := env.appendLogEvents(1)
	b, err := env.client.Events().Poll(env.ctx, model.Poll{Call: model.Call{Correlation: "poll"}, Subscription: res.Subscription.Ref})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	requireEventIDs(t, "subscription after the list", b.Events, append(between, after...))
}

// eventListHidesUnreadable lists an internal note's events as a member,
// who may not read them, and as staff: the member's page omits them
// without placeholder or count, and both pages end at the same position.
// A page of only hidden events is empty and still advances.
func eventListHidesUnreadable(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	note := env.note("staff only")
	visible := env.appendLogEvents(1)
	hidden := note.Operation.ResultMessage.Ref()

	member := env.listEvents(env.client, eventListReq(model.StartCheckpoint, tail, 0))
	for _, ev := range member.Items {
		if ev.Resource == hidden {
			t.Fatalf("member listed event %s of the internal note", ev.ID)
		}
	}
	if !slices.Contains(eventIDsOf(member.Items), visible[0].ID) {
		t.Fatalf("member page %v lacks the public event %s", eventIDsOf(member.Items), visible[0].ID)
	}

	env.policy.setStaff(true)
	staff := env.listEvents(env.client, eventListReq(model.StartCheckpoint, tail, 0))
	env.policy.setStaff(false)
	var noteEvents int
	for _, ev := range staff.Items {
		if ev.Resource == hidden {
			noteEvents++
		}
	}
	if noteEvents == 0 || len(member.Items) != len(staff.Items)-noteEvents {
		t.Fatalf("staff page %d items with %d note events, member page %d; want the member page without exactly those",
			len(staff.Items), noteEvents, len(member.Items))
	}
	if member.Next != staff.Next || member.Complete != staff.Complete {
		t.Fatalf("member page ends at %q (complete %v), staff at %q (complete %v); want the same position",
			member.Next, member.Complete, staff.Next, staff.Complete)
	}

	// Page one event at a time: every page of a note event is empty for
	// the member, never a placeholder, and the pages still reach the head.
	at, pages := tail, 0
	for range len(staff.Items) + 1 {
		p := env.listEvents(env.client, eventListReq(model.StartCheckpoint, at, 1))
		for _, ev := range p.Items {
			if ev.Resource == hidden {
				t.Fatalf("member listed event %s of the internal note", ev.ID)
			}
		}
		if p.Next == at && !p.Complete {
			t.Fatal("a page of hidden events did not advance")
		}
		at, pages = p.Next, pages+1
		if p.Complete {
			break
		}
	}
	if at != staff.Next || pages < len(staff.Items) {
		t.Fatalf("one-event pages ended at %q after %d pages, want %q after at least %d", at, pages, staff.Next, len(staff.Items))
	}
}

// eventListDenied denies event.list: List is Unauthorized and opens no
// store unit, whatever the start.
func eventListDenied(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	env.appendLogEvents(1)
	env.policy.deny(model.ActionEventList)
	counting, e := env.countingEngine()
	c := env.clientOf(e, serviceTenant, "staff-1")
	counting.reset()
	for _, req := range []model.ListEvents{
		eventListReq(model.StartBeginning, "", 0),
		eventListReq(model.StartTail, "", 0),
		eventListReq(model.StartCheckpoint, tail, 0),
	} {
		_, err := c.Events().List(env.ctx, req)
		serviceCode(t, err, model.Unauthorized)
	}
	if v, w := counting.views.Load(), counting.transacts.Load(); v+w != 0 {
		t.Fatalf("denied List opened %d read and %d write units, want none", v, w)
	}
}

// eventListReadOnly lists through an engine that counts write units: none
// is opened, and the event log head does not move.
func eventListReadOnly(t *testing.T, env *serviceEnv) {
	env.note("internal")
	env.appendLogEvents(2)
	counting, e := env.countingEngine()
	c := env.clientOf(e, serviceTenant, "staff-1")
	head := env.eventHead()
	counting.reset()
	p := env.listEvents(c, eventListReq(model.StartBeginning, "", 1))
	env.listEvents(c, eventListReq(model.StartCheckpoint, p.Next, 0))
	env.listEvents(c, eventListReq(model.StartTail, "", 0))
	if n := counting.transacts.Load(); n != 0 {
		t.Fatalf("List opened %d write units, want none", n)
	}
	if got := env.eventHead(); got != head {
		t.Fatalf("event head moved %q -> %q, want no event", head, got)
	}
	env.requireNoSubscriptionRows()
}

// eventListCursorExpired purges events past the horizon: a position
// behind the retained beginning is CursorExpired with List's recovery,
// and listing from the beginning restarts at the retained history.
func eventListCursorExpired(t *testing.T, env *serviceEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	old := env.listEvents(env.client, eventListReq(model.StartBeginning, "", 1))
	env.fx.Advance(2 * serviceHorizon)
	env.clock.Advance(2 * serviceHorizon)
	recent := env.appendLogEvents(1)
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "staff-1"}, Call: model.Call{Correlation: "purge"}}
	if rep, err := env.engine.Runtime().Purge(env.ctx, call); err != nil || rep.Events < 2 {
		t.Fatalf("Purge = %+v, %v; want the seeded events purged", rep, err)
	}

	_, err := env.client.Events().List(env.ctx, eventListReq(model.StartCheckpoint, old.Next, 0))
	serviceCode(t, err, model.CursorExpired)
	var me *model.Error
	if !errors.As(err, &me) || me.Retry != model.ReconcileFirst ||
		!strings.Contains(me.SafeMessage, "restart the list from beginning or tail") {
		t.Fatalf("expired List error = %+v, want List's recovery", me)
	}
	restarted := env.listEvents(env.client, eventListReq(model.StartBeginning, "", 0))
	requireEventIDs(t, "retained history", restarted.Items, recent)
	env.requireNoSubscriptionRows()
}

// eventListLimitCap scans DefaultEventListLimit events for a zero Limit
// and at most MaxEventListLimit for a larger one.
func eventListLimitCap(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	evs := env.appendLogEvents(uma.MaxEventListLimit + 1)
	def := env.listEvents(env.client, eventListReq(model.StartCheckpoint, tail, 0))
	requireEventIDs(t, "default page", def.Items, evs[:uma.DefaultEventListLimit])
	capped := env.listEvents(env.client, eventListReq(model.StartCheckpoint, tail, 10*uma.MaxEventListLimit))
	requireEventIDs(t, "capped page", capped.Items, evs[:uma.MaxEventListLimit])
	if capped.Complete {
		t.Fatal("capped page reported complete")
	}
	last := env.listEvents(env.client, eventListReq(model.StartCheckpoint, capped.Next, 10*uma.MaxEventListLimit))
	requireEventIDs(t, "last page", last.Items, evs[uma.MaxEventListLimit:])
	if !last.Complete {
		t.Fatal("last page not complete")
	}
}
