package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// workflowCases pin Reader.Conversations on the workflow filters: the
// assignee (actor or none; Self is the service's to resolve and a store
// refuses it) and the snooze predicate, judged at the store clock, which a
// live inbound message recorded after the snooze was set ends (an imported
// one does not), and which the store evaluates itself so a filtered page is
// full.
var workflowCases = []storeCase{
	{"AssigneeSelectsActorOrNone", func(t *testing.T, e *env) {
		desk, other := model.ActorID("desk-1"), model.ActorID("desk-2")
		a := e.putWorkflowConversation(t, &desk, nil)
		b := e.putWorkflowConversation(t, &other, nil)
		c := e.putWorkflowConversation(t, nil, nil)
		e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{Actor: desk}}, a)
		e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{Actor: other}}, b)
		e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{None: true}}, c)
		e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{Actor: "desk-3"}})
		err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
			_, err := r.Conversations(e.ctx, listReq(model.Filter{Assignee: &model.AssigneeFilter{Self: true}}, model.PageRequest{Limit: 10}))
			return err
		})
		wantCode(t, err, model.Validation)
	}},
	{"SnoozeWakesOnLiveInbound", func(t *testing.T, e *env) {
		set := e.at(100)
		snoozed := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(24 * time.Hour).UTC(), At: set, By: "desk-1"})
		plain := e.putWorkflowConversation(t, nil, nil)
		yes, no := true, false
		check := func(step string, inEffect bool) {
			t.Helper()
			t.Run(step, func(t *testing.T) {
				if inEffect {
					e.wantConversations(t, model.Filter{Snoozed: &yes}, snoozed)
					e.wantConversations(t, model.Filter{Snoozed: &no}, plain)
					return
				}
				e.wantConversations(t, model.Filter{Snoozed: &yes})
				e.wantConversations(t, model.Filter{Snoozed: &no}, snoozed, plain)
			})
		}
		check("set", true)
		e.putWorkflowMessage(t, snoozed, set.Add(-time.Second), nil)
		check("inbound before", true)
		e.putWorkflowMessage(t, snoozed, set, nil)
		check("inbound at the same instant", true)
		e.putWorkflowMessage(t, snoozed, set.Add(time.Second), func(m *model.Message) {
			m.Origin = &model.Origin{Actor: "desk-1"}
		})
		check("outbound after", true)
		e.putWorkflowMessage(t, snoozed, set.Add(time.Second), func(m *model.Message) { m.Redacted = true })
		check("redacted inbound after", true)
		e.putWorkflowMessage(t, snoozed, set.Add(time.Second), func(m *model.Message) { m.Deleted = true })
		check("deleted inbound after", true)
		e.putWorkflowMessage(t, plain, set.Add(time.Second), nil)
		check("inbound after in another conversation", true)
		e.putWorkflowMessage(t, snoozed, set.Add(time.Second), func(m *model.Message) { m.ImportedBy = "desk-1" })
		check("imported inbound after", true)
		e.putWorkflowMessage(t, snoozed, set.Add(time.Nanosecond), nil)
		check("inbound after", false)
	}},
	{"SnoozeExpiresAtStoreClock", func(t *testing.T, e *env) {
		if e.fx.Advance == nil {
			t.Skip("fixture cannot advance the store clock")
		}
		until := e.fx.Now().Add(time.Hour).UTC()
		c := e.putWorkflowConversation(t, nil, &model.Snooze{Until: until, At: e.at(0), By: "desk-1"})
		yes, no := true, false
		e.wantConversations(t, model.Filter{Snoozed: &yes}, c)
		e.fx.Advance(time.Hour)
		e.wantConversations(t, model.Filter{Snoozed: &yes})
		e.wantConversations(t, model.Filter{Snoozed: &no}, c)
	}},
	{"FilteredPagesAreFull", func(t *testing.T, e *env) {
		desk := model.ActorID("desk-1")
		var want []model.ConversationRef
		for i := range 9 {
			if i%3 == 0 {
				want = append(want, e.putWorkflowConversation(t, &desk, nil))
				continue
			}
			e.putWorkflowConversation(t, nil, nil)
		}
		f := model.Filter{Assignee: &model.AssigneeFilter{Actor: desk}}
		var got []model.ConversationRef
		var after model.Cursor
		for range 10 {
			var pg model.Page[model.Conversation]
			e.mustView(t, func(r store.Reader) (err error) {
				pg, err = r.Conversations(e.ctx, listReq(f, model.PageRequest{Limit: 1, After: after}))
				return err
			})
			if !pg.Complete && len(pg.Items) != 1 {
				t.Fatalf("page after %q holds %d items without completing, want a full page", after, len(pg.Items))
			}
			for _, c := range pg.Items {
				got = append(got, c.Ref)
			}
			if pg.Complete {
				break
			}
			after = pg.Next
		}
		if len(got) != len(want) {
			t.Fatalf("paged %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("paged %v, want %v in recording order", got, want)
			}
		}
	}},
	{"WorkflowStateRoundTrips", func(t *testing.T, e *env) {
		desk := model.ActorID("desk-1")
		s := &model.Snooze{Until: time.Unix(1_800_000_000, 123456789).UTC(), At: time.Unix(1_700_000_100, 987654321).UTC(), By: desk}
		ref := e.putWorkflowConversation(t, &desk, s)
		var got model.Conversation
		e.mustView(t, func(r store.Reader) (err error) {
			got, err = r.GetConversation(e.ctx, ref)
			return err
		})
		if got.Assignee == nil || *got.Assignee != desk || got.Snooze == nil || *got.Snooze != *s {
			t.Fatalf("stored assignee %v snooze %+v, want %v %+v", got.Assignee, got.Snooze, desk, s)
		}
		next := got.Clone()
		next.Assignee, next.Snooze, next.Revision = nil, nil, "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, next, got.Revision) })
		yes := true
		e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{Actor: desk}})
		e.wantConversations(t, model.Filter{Snoozed: &yes})
		e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{None: true}}, ref)
	}},
}

// conversationLinkFilterCases pin the Linked and Unread filters of Reader.Conversations.
var conversationLinkFilterCases = []storeCase{
	{"LinkedSelectsHoldersOrNot", func(t *testing.T, e *env) {
		a := e.putWorkflowConversation(t, nil, nil)
		b := e.putWorkflowConversation(t, nil, nil)
		c := e.putWorkflowConversation(t, nil, nil)
		e.setLinks(t, a, model.ConversationLink{Conversation: &b})
		e.setLinks(t, b, model.ConversationLink{Conversation: &a})
		yes, no := true, false
		e.wantConversations(t, model.Filter{Linked: &yes}, a, b)
		e.wantConversations(t, model.Filter{Linked: &no}, c)
		e.setLinks(t, a)
		e.wantConversations(t, model.Filter{Linked: &yes}, b)
	}},
	{"UnreadPerActor", func(t *testing.T, e *env) {
		a := e.putWorkflowConversation(t, nil, nil)
		b := e.putWorkflowConversation(t, nil, nil)
		empty := e.putWorkflowConversation(t, nil, nil)
		e.putWorkflowMessage(t, a, e.at(10), nil)
		e.putWorkflowMessage(t, b, e.at(10), nil)
		one := model.UnreadFilter{Actor: "desk-1"}
		two := model.UnreadFilter{Actor: "desk-2"}
		e.wantConversations(t, model.Filter{Unread: &one}, a, b)
		e.putMarker(t, a, "desk-1", e.at(10), "")
		e.wantConversations(t, model.Filter{Unread: &one}, b)
		e.wantConversations(t, model.Filter{Unread: &two}, a, b)
		// A message recorded after the marker is unread again; the actor's
		// own uma-authored message and an echo are not.
		e.putWorkflowMessage(t, a, e.at(11), func(m *model.Message) { m.Origin = &model.Origin{Actor: "desk-1"} })
		e.putWorkflowMessage(t, a, e.at(12), func(m *model.Message) { m.EchoOf = &model.MessageRef{Conversation: a, ID: e.id(t)} })
		e.wantConversations(t, model.Filter{Unread: &one}, b)
		e.putWorkflowMessage(t, a, e.at(13), func(m *model.Message) { m.Origin = &model.Origin{Actor: "desk-2"} })
		e.wantConversations(t, model.Filter{Unread: &one}, a, b)
		e.putMarker(t, a, "desk-1", e.at(12), "m1")
		e.wantConversations(t, model.Filter{Unread: &one}, a, b)
		e.putMarker(t, a, "desk-1", e.at(13), "m1m")
		e.wantConversations(t, model.Filter{Unread: &one}, b)
		// An imported message (Message.ImportedBy) is never unread: not
		// after the marker, nor where the actor has none.
		e.putWorkflowMessage(t, a, e.at(14), func(m *model.Message) { m.ImportedBy = "desk-2" })
		e.putWorkflowMessage(t, empty, e.at(14), func(m *model.Message) { m.ImportedBy = "desk-2" })
		e.wantConversations(t, model.Filter{Unread: &one}, b)
		e.wantConversations(t, model.Filter{Unread: &two}, a, b)
		err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
			_, err := r.Conversations(e.ctx, listReq(model.Filter{Unread: &model.UnreadFilter{}}, model.PageRequest{Limit: 10}))
			return err
		})
		wantCode(t, err, model.Validation)
	}},
}

// setLinks replaces conv's links.
func (e *env) setLinks(t *testing.T, conv model.ConversationRef, links ...model.ConversationLink) {
	t.Helper()
	e.mustTx(t, func(tx store.Tx) error {
		c, err := tx.GetConversation(e.ctx, conv)
		if err != nil {
			return err
		}
		next := c.Clone()
		next.Links = links
		next.Revision = c.Revision + "+"
		return tx.PutConversation(e.ctx, next, c.Revision)
	})
}

// putMarker writes actor's marker on conv reading through at, replacing
// the one at expected ("" for none) with revision expected+"m".
func (e *env) putMarker(t *testing.T, conv model.ConversationRef, actor model.ActorID, at time.Time, expected model.Revision) {
	t.Helper()
	rev := model.Revision("m1")
	if expected != "" {
		rev = expected + "m"
	}
	e.mustTx(t, func(tx store.Tx) error {
		return tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: conv, Actor: actor, ReadThrough: at, Revision: rev}, expected)
	})
}

// putWorkflowConversation stores an open conversation in the env's inbox
// with assignee and snooze.
func (e *env) putWorkflowConversation(t *testing.T, assignee *model.ActorID, snooze *model.Snooze) model.ConversationRef {
	t.Helper()
	ref := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	c := e.conversation(ref, "r1")
	c.Assignee, c.Snooze = assignee, snooze
	e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, c, "") })
	return ref
}

// putWorkflowMessage stores an inbound message in conv recorded at at,
// changed by edit when set.
func (e *env) putWorkflowMessage(t *testing.T, conv model.ConversationRef, at time.Time, edit func(*model.Message)) {
	t.Helper()
	m := e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "r1")
	m.RecordedAt, m.OccurredAt = at, at
	if edit != nil {
		edit(&m)
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, m, "") })
}

// wantConversations requires Conversations under f, among the env inbox's,
// to be exactly want in recording order.
func (e *env) wantConversations(t *testing.T, f model.Filter, want ...model.ConversationRef) {
	t.Helper()
	f.Inboxes = []model.InboxRef{e.inbox}
	var got []model.ConversationRef
	e.mustView(t, func(r store.Reader) error {
		pg, err := r.Conversations(e.ctx, listReq(f, model.PageRequest{Limit: 100}))
		for _, c := range pg.Items {
			got = append(got, c.Ref)
		}
		return err
	})
	if len(got) != len(want) {
		t.Fatalf("Conversations(%+v) = %v, want %v", f, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Conversations(%+v) = %v, want %v", f, got, want)
		}
	}
}
