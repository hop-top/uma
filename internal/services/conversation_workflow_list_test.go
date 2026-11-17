package services_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// conversations stores n more open conversations in the rig's inbox and
// returns their references, the rig's own first, in recording order.
func (w *workflowRig) conversations(t *testing.T, n int) []model.ConversationRef {
	t.Helper()
	refs := []model.ConversationRef{w.Conv.Ref}
	if err := w.E.Store().Transact(w.Ctx, tenantA, func(tx store.Tx) error {
		for i := range n {
			c := w.Conv.Clone()
			c.Ref.ID = model.ID(fmt.Sprintf("00000004-0000-7000-8000-%012d", i+1))
			c.ReplyRoute.ID = fmt.Sprintf("room-%d", i+2)
			if err := tx.PutConversation(w.Ctx, c, ""); err != nil {
				return err
			}
			refs = append(refs, c.Ref)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return refs
}

func (w *workflowRig) assign(t *testing.T, conv model.ConversationRef, to model.ActorID) {
	t.Helper()
	if _, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: conv, Assignee: to}); err != nil {
		t.Fatalf("Assign %s: %v", conv.ID, err)
	}
}

func (w *workflowRig) snooze(t *testing.T, conv model.ConversationRef, d time.Duration) {
	t.Helper()
	until := w.Clock.Now().Add(d)
	if _, err := w.desk.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: conv, Until: &until}); err != nil {
		t.Fatalf("Snooze %s: %v", conv.ID, err)
	}
}

// listAll pages Conversations.List under f at limit and returns the refs
// in order, requiring every page but the last to be full.
func listAll(t *testing.T, c enginetest.Client, w *workflowRig, f model.Filter, limit int) []model.ConversationRef {
	t.Helper()
	var out []model.ConversationRef
	var after model.Cursor
	for range 50 {
		pg, err := c.Conversations().List(w.Ctx, model.ListRequest{Call: call(), Filter: f, Page: model.PageRequest{Limit: limit, After: after}})
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		for _, conv := range pg.Items {
			out = append(out, conv.Ref)
		}
		if pg.Complete || pg.Next == "" {
			return out
		}
		if len(pg.Items) != limit {
			t.Fatalf("List(%+v) page of %d items before the end, want full pages of %d", f, len(pg.Items), limit)
		}
		after = pg.Next
	}
	t.Fatal("List did not complete")
	return nil
}

// Filter.Assignee selects by actor, by the caller (Self, per principal)
// or the unassigned, server-side, in full stable pages.
func TestListByAssignee(t *testing.T) {
	w := newWorkflowRig(t)
	refs := w.conversations(t, 9)
	var mine, theirs, none []model.ConversationRef
	for i, ref := range refs {
		switch i % 3 {
		case 0:
			w.assign(t, ref, deskActor)
			mine = append(mine, ref)
		case 1:
			w.assign(t, ref, "desk-2")
			theirs = append(theirs, ref)
		default:
			none = append(none, ref)
		}
	}
	for name, tc := range map[string]struct {
		c    enginetest.Client
		f    model.AssigneeFilter
		want []model.ConversationRef
	}{
		"actor":           {w.desk, model.AssigneeFilter{Actor: "desk-2"}, theirs},
		"self":            {w.desk, model.AssigneeFilter{Self: true}, mine},
		"self of another": {clientAs(t, w.rig, "desk-2"), model.AssigneeFilter{Self: true}, theirs},
		"none":            {w.desk, model.AssigneeFilter{None: true}, none},
	} {
		t.Run(name, func(t *testing.T) {
			for _, limit := range []int{1, 2, 50} {
				f := tc.f
				if got := listAll(t, tc.c, w, model.Filter{Assignee: &f}, limit); !slices.Equal(got, tc.want) {
					t.Fatalf("limit %d: %v, want %v", limit, got, tc.want)
				}
			}
		})
	}
}

// Filter.Snoozed keeps the conversations whose snooze is in effect, or the
// others; a conversation leaves the snoozed set when an inbound message
// arrives and when its snooze expires.
func TestListBySnooze(t *testing.T) {
	w := newWorkflowRig(t)
	refs := w.conversations(t, 3)
	w.snooze(t, refs[0], time.Hour)
	w.snooze(t, refs[2], 2*time.Hour)
	yes, no := true, false
	check := func(step string, snoozed, awake []model.ConversationRef) {
		t.Helper()
		if got := listAll(t, w.desk, w, model.Filter{Snoozed: &yes}, 1); !slices.Equal(got, snoozed) {
			t.Fatalf("%s: snoozed %v, want %v", step, got, snoozed)
		}
		if got := listAll(t, w.desk, w, model.Filter{Snoozed: &no}, 1); !slices.Equal(got, awake) {
			t.Fatalf("%s: awake %v, want %v", step, got, awake)
		}
	}
	check("set", []model.ConversationRef{refs[0], refs[2]}, []model.ConversationRef{refs[1], refs[3]})
	w.Clock.Advance(time.Second)
	w.inbound(t, "00000003-0000-7000-8000-0000000000a1") // in refs[0], the rig's conversation
	check("inbound", []model.ConversationRef{refs[2]}, []model.ConversationRef{refs[0], refs[1], refs[3]})
	w.Clock.Advance(2 * time.Hour)
	check("expired", nil, refs)
}

// A principal without conversation.read_workflow receives nothing through
// a workflow filter, not even the unassigned or awake conversations.
func TestWorkflowFiltersHiddenFromExternal(t *testing.T) {
	w := newWorkflowRig(t)
	w.conversations(t, 2)
	ext := clientAs(t, w.rig, externalActor)
	no := false
	for name, f := range map[string]model.Filter{
		"none":   {Assignee: &model.AssigneeFilter{None: true}},
		"self":   {Assignee: &model.AssigneeFilter{Self: true}},
		"awake":  {Snoozed: &no},
		"plain?": {},
	} {
		got := listAll(t, ext, w, f, 10)
		if name == "plain?" {
			if len(got) != 3 {
				t.Fatalf("external plain list %v, want all three", got)
			}
			continue
		}
		if len(got) != 0 {
			t.Fatalf("%s: external receives %v through a workflow filter", name, got)
		}
	}
}

// A cursor is bound to the workflow filter it was issued for, and Self is
// bound as the actor it resolves to: another filter, or the same Self
// filter from another principal, refuses it.
func TestWorkflowFilterCursorBinding(t *testing.T) {
	w := newWorkflowRig(t)
	for _, ref := range w.conversations(t, 3) {
		w.assign(t, ref, deskActor)
	}
	self := model.Filter{Assignee: &model.AssigneeFilter{Self: true}}
	pg, err := w.desk.Conversations().List(w.Ctx, model.ListRequest{Call: call(), Filter: self, Page: model.PageRequest{Limit: 1}})
	if err != nil || pg.Next == "" {
		t.Fatalf("first page = %+v, %v", pg, err)
	}
	yes := true
	for name, tc := range map[string]struct {
		c enginetest.Client
		f model.Filter
	}{
		"snoozed":         {w.desk, model.Filter{Snoozed: &yes}},
		"other actor":     {w.desk, model.Filter{Assignee: &model.AssigneeFilter{Actor: "desk-2"}}},
		"self of another": {clientAs(t, w.rig, "desk-2"), self},
	} {
		_, err := tc.c.Conversations().List(w.Ctx, model.ListRequest{Call: call(), Filter: tc.f, Page: model.PageRequest{Limit: 1, After: pg.Next}})
		requireCode(t, err, model.Validation, "page.after")
		_ = name
	}
	explicit := model.Filter{Assignee: &model.AssigneeFilter{Actor: deskActor}}
	if _, err := w.desk.Conversations().List(w.Ctx, model.ListRequest{Call: call(), Filter: explicit, Page: model.PageRequest{Limit: 1, After: pg.Next}}); err != nil {
		t.Fatalf("Self cursor refused for its resolved actor: %v", err)
	}
	// A filter without workflow fields encodes as before they existed.
	plain := model.Filter{Statuses: []string{model.ConversationOpen}}
	withWorkflow := plain
	withWorkflow.Snoozed = &yes
	if slices.Equal(services.CanonicalFilter(plain), services.CanonicalFilter(withWorkflow)) {
		t.Fatal("the snoozed field does not reach the canonical filter")
	}
}
