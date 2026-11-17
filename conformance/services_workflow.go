package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// workflowServiceCases run conversation workflow state through the
// services over the store: assignment at a revision, snooze waking on an
// inbound message and at expiry, the read gate, and the workflow filters
// evaluated by the store in full pages.
var workflowServiceCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"AssignStaleRevisionConflicts", func(t *testing.T, env *serviceEnv) {
		cv := env.client.Conversations()
		if _, err := cv.Assign(env.ctx, model.ConversationAssignment{Command: env.cmd(env.conv.Revision), Conversation: env.conv.Ref, Assignee: "desk-1"}); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		_, err := cv.Assign(env.ctx, model.ConversationAssignment{Command: env.cmd(env.conv.Revision), Conversation: env.conv.Ref, Assignee: "desk-2"})
		wantCode(t, err, model.Conflict)
		got := env.getConversation(t)
		if got.Assignee == nil || *got.Assignee != "desk-1" || got.Revision == env.conv.Revision {
			t.Fatalf("conversation %+v, want assigned to desk-1 at a new revision", got)
		}
		if _, err := cv.Unassign(env.ctx, model.ConversationAssignment{Command: env.cmd(got.Revision), Conversation: env.conv.Ref}); err != nil {
			t.Fatalf("Unassign: %v", err)
		}
		if got := env.getConversation(t); got.Assignee != nil {
			t.Fatalf("assignee %v after Unassign", *got.Assignee)
		}
	}},
	{"SnoozeWakesOnInbound", func(t *testing.T, env *serviceEnv) {
		env.snoozeConversation(t, time.Hour)
		yes, no := true, false
		env.wantListed(t, model.Filter{Snoozed: &yes}, env.conv.Ref)
		if got := env.getConversation(t); got.Snooze == nil {
			t.Fatal("snooze not shown while in effect")
		}
		env.clock.Advance(time.Second)
		env.inboundMessage(t, env.clock.Now().UTC())
		env.wantListed(t, model.Filter{Snoozed: &yes})
		env.wantListed(t, model.Filter{Snoozed: &no}, env.conv.Ref)
		if got := env.getConversation(t); got.Snooze != nil {
			t.Fatalf("snooze %+v shown after an inbound message", got.Snooze)
		}
	}},
	{"SnoozeWakesOnExpiry", func(t *testing.T, env *serviceEnv) {
		if env.fx.Advance == nil {
			t.Skip("fixture cannot advance the store clock")
		}
		env.snoozeConversation(t, time.Hour)
		yes := true
		env.wantListed(t, model.Filter{Snoozed: &yes}, env.conv.Ref)
		env.fx.Advance(time.Hour)
		env.clock.Advance(time.Hour)
		env.wantListed(t, model.Filter{Snoozed: &yes})
		if got := env.getConversation(t); got.Snooze != nil {
			t.Fatalf("snooze %+v shown after expiry", got.Snooze)
		}
	}},
	{"ReaderWithoutWorkflowSeesNeither", func(t *testing.T, env *serviceEnv) {
		if _, err := env.client.Conversations().Assign(env.ctx, model.ConversationAssignment{Command: env.cmd(""), Conversation: env.conv.Ref, Assignee: "desk-1"}); err != nil {
			t.Fatal(err)
		}
		env.snoozeConversation(t, time.Hour)
		env.policy.deny(model.ActionConversationReadWorkflow)
		if got := env.getConversation(t); got.Assignee != nil || got.Snooze != nil {
			t.Fatalf("Get shows %+v without conversation.read_workflow", got)
		}
		pg, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Inboxes: []model.InboxRef{env.inbox}}, 10, ""))
		if err != nil || len(pg.Items) != 1 || pg.Items[0].Assignee != nil || pg.Items[0].Snooze != nil {
			t.Fatalf("List = %+v, %v, want the conversation without workflow state", pg, err)
		}
		env.wantListed(t, model.Filter{Assignee: &model.AssigneeFilter{Actor: "desk-1"}})
		events, err := env.client.Events().List(env.ctx, model.ListEvents{Call: model.Call{Correlation: "corr"}, Start: model.StartBeginning, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events.Items {
			if model.IsWorkflowEvent(ev.Kind) {
				t.Fatalf("workflow event %s listed without conversation.read_workflow", ev.Kind)
			}
		}
	}},
	{"FiltersServerSideFullPages", func(t *testing.T, env *serviceEnv) {
		var want []model.ConversationRef
		for i := range 7 {
			c := env.conversation(env.inbox, model.ConversationOpen)
			if i%3 == 1 {
				if _, err := env.client.Conversations().Assign(env.ctx, model.ConversationAssignment{Command: env.cmd(""), Conversation: c.Ref, Assignee: "staff-1"}); err != nil {
					t.Fatal(err)
				}
				want = append(want, c.Ref)
			}
		}
		f := model.Filter{Inboxes: []model.InboxRef{env.inbox}, Assignee: &model.AssigneeFilter{Self: true}}
		var got []model.ConversationRef
		var after model.Cursor
		for range 20 {
			pg, err := env.client.Conversations().List(env.ctx, serviceList(f, 1, after))
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, c := range pg.Items {
				got = append(got, c.Ref)
			}
			if pg.Complete || pg.Next == "" {
				break
			}
			if len(pg.Items) != 1 {
				t.Fatalf("page of %d items before the end, want full pages", len(pg.Items))
			}
			after = pg.Next
		}
		if len(got) != len(want) {
			t.Fatalf("listed %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("listed %v, want %v in recording order", got, want)
			}
		}
	}},
}

func (env *serviceEnv) getConversation(t *testing.T) model.Conversation {
	t.Helper()
	got, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "corr"}, Ref: env.conv.Ref})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got
}

func (env *serviceEnv) snoozeConversation(t *testing.T, d time.Duration) {
	t.Helper()
	until := env.clock.Now().Add(d)
	if _, err := env.client.Conversations().Snooze(env.ctx, model.ConversationSnooze{Command: env.cmd(""), Conversation: env.conv.Ref, Until: &until}); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
}

// inboundMessage records a live inbound message in the env's conversation
// at at.
func (env *serviceEnv) inboundMessage(t *testing.T, at time.Time) {
	t.Helper()
	m := model.Message{
		Ref: model.MessageRef{Conversation: env.conv.Ref, ID: env.id()}, Audience: model.Audience{Visibility: model.Public},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "still there?"}}},
		OccurredAt: at, RecordedAt: at, Revision: "m1",
	}
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") })
}

// wantListed requires Conversations.List under f, in the env's inbox, to
// be exactly want.
func (env *serviceEnv) wantListed(t *testing.T, f model.Filter, want ...model.ConversationRef) {
	t.Helper()
	f.Inboxes = []model.InboxRef{env.inbox}
	pg, err := env.client.Conversations().List(env.ctx, serviceList(f, 100, ""))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pg.Items) != len(want) {
		t.Fatalf("List lists %d conversations, want %v", len(pg.Items), want)
	}
	for i := range want {
		if pg.Items[i].Ref != want[i] {
			t.Fatalf("List item %d = %v, want %v", i, pg.Items[i].Ref, want[i])
		}
	}
}

// linkServiceCases run links and read markers through the services over
// the store.
var linkServiceCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"LinkBothEndsHidesUnreadableFarEnd", func(t *testing.T, env *serviceEnv) {
		other := env.conversation(env.inbox, model.ConversationOpen)
		cv := env.client.Conversations()
		if _, err := cv.Link(env.ctx, model.ConversationLinkChange{Command: env.cmd(""), Conversation: env.conv.Ref, Link: model.ConversationLink{Conversation: &other.Ref}}); err != nil {
			t.Fatalf("Link: %v", err)
		}
		far, err := cv.Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "corr"}, Ref: other.Ref})
		if err != nil || len(far.Links) != 1 || *far.Links[0].Conversation != env.conv.Ref {
			t.Fatalf("far end %+v, %v, want it to hold the link", far, err)
		}
		yes := true
		env.wantListed(t, model.Filter{Linked: &yes}, env.conv.Ref, other.Ref)
		env.policy.denyOn(model.ActionConversationRead, other.Ref.Ref())
		if got := env.getConversation(t); len(got.Links) != 0 {
			t.Fatalf("links %+v shown with an unreadable far end", got.Links)
		}
		env.wantListed(t, model.Filter{Linked: &yes})
		env.policy.denyOn(model.ActionConversationLink, env.conv.Ref.Ref())
		_, err = cv.Unlink(env.ctx, model.ConversationLinkChange{Command: env.cmd(""), Conversation: other.Ref, Link: model.ConversationLink{Conversation: &env.conv.Ref}})
		wantCode(t, err, model.Unauthorized)
	}},
	{"ReadStatePerActor", func(t *testing.T, env *serviceEnv) {
		env.clock.Advance(time.Second)
		env.inboundMessage(t, env.clock.Now().UTC())
		env.conversation(env.inbox, model.ConversationOpen) // no message: never unread
		env.clock.Advance(time.Second)
		before := env.getConversation(t).Revision
		if _, err := env.client.Conversations().MarkRead(env.ctx, model.ReadMark{Call: model.Call{Correlation: "corr"}, Conversation: env.conv.Ref}); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		if got := env.getConversation(t).Revision; got != before {
			t.Fatalf("revision %q after MarkRead, want %q", got, before)
		}
		desk2, err := env.engine.For(model.Principal{Tenant: serviceTenant, Actor: "staff-2"})
		if err != nil {
			t.Fatal(err)
		}
		mine := model.Filter{Inboxes: []model.InboxRef{env.inbox}, Unread: &model.UnreadFilter{}}
		pg, err := env.client.Conversations().List(env.ctx, serviceList(mine, 10, ""))
		if err != nil || len(pg.Items) != 0 {
			t.Fatalf("staff-1 unread = %+v, %v, want none", pg.Items, err)
		}
		pg, err = desk2.Conversations().List(env.ctx, serviceList(mine, 10, ""))
		if err != nil || len(pg.Items) != 1 || pg.Items[0].Ref != env.conv.Ref {
			t.Fatalf("staff-2 unread = %+v, %v, want the conversation with a message", pg.Items, err)
		}
		env.policy.deny(model.ActionConversationMarkRead)
		_, err = desk2.Conversations().MarkRead(env.ctx, model.ReadMark{Call: model.Call{Correlation: "corr"}, Conversation: env.conv.Ref})
		wantCode(t, err, model.Unauthorized)
	}},
}
