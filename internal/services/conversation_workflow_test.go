package services_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

const viewerActor model.ActorID = "viewer-1"

// workflowPolicy grants the rig's actor and a desk actor everything, a
// viewer every read (workflow state included) but no change, and an
// external participant conversation.read at the public audience only.
type workflowPolicy struct {
	mu     sync.Mutex
	checks []policy.Authorization
}

func (p *workflowPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	p.checks = append(p.checks, a)
	p.mu.Unlock()
	public := a.Audience.Visibility == "" || a.Audience.Visibility == model.Public
	switch a.Principal.Actor {
	case externalActor:
		return policy.Decision{Allow: public && (a.Action == model.ActionConversationRead || a.Action == model.ActionInboxRead)}, nil
	case viewerActor:
		switch a.Action {
		case model.ActionConversationRead, model.ActionConversationReadWorkflow, model.ActionInboxRead, model.ActionEventList:
			return policy.Decision{Allow: true}, nil
		}
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (p *workflowPolicy) asked(action string) []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []policy.Authorization
	for _, a := range p.checks {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

type workflowRig struct {
	*rig
	pol  *workflowPolicy
	desk enginetest.Client
	n    int
}

func newWorkflowRig(t *testing.T) *workflowRig {
	t.Helper()
	pol := &workflowPolicy{}
	r := newRig(t, withPolicy(pol))
	return &workflowRig{rig: r, pol: pol, desk: clientAs(t, r, deskActor)}
}

func (w *workflowRig) cmd(rev model.Revision) model.Command {
	w.n++
	return model.Command{Call: call(), Operation: model.OperationID("op-wf-" + strconv.Itoa(w.n)), ExpectedRevision: rev}
}

func (w *workflowRig) get(t *testing.T, c enginetest.Client) model.Conversation {
	t.Helper()
	conv, err := c.Conversations().Get(w.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: w.Conv.Ref})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return conv
}

func (w *workflowRig) stored(t *testing.T) model.Conversation {
	t.Helper()
	var conv model.Conversation
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		conv, err = r.GetConversation(w.Ctx, w.Conv.Ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return conv
}

// events returns the workflow events in the log, oldest first.
func (w *workflowRig) events(t *testing.T) []model.Event {
	t.Helper()
	var out []model.Event
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) error {
		pg, err := r.Events(w.Ctx, model.Filter{}, "", 100)
		for _, ev := range pg.Items {
			if model.IsWorkflowEvent(ev.Kind) {
				out = append(out, ev)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// inbound records a live inbound message in the rig's conversation at the
// clock's now.
func (w *workflowRig) inbound(t *testing.T, id model.ID) {
	t.Helper()
	m := model.Message{
		Ref: model.MessageRef{Conversation: w.Conv.Ref, ID: id}, Audience: model.Audience{Visibility: model.Public},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello again"}}},
		OccurredAt: w.Clock.Now().UTC(), RecordedAt: w.Clock.Now().UTC(), Revision: "m1",
	}
	if err := w.E.Store().Transact(w.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(w.Ctx, m, "") }); err != nil {
		t.Fatal(err)
	}
}

func payloadOf[T any](t *testing.T, ev model.Event) T {
	t.Helper()
	var p T
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Assign sets the assignee at a new revision with one assigned event
// carrying references only; assigning the same actor again changes
// nothing; a stale expected revision conflicts and changes nothing.
func TestAssignAndStaleRevision(t *testing.T) {
	w := newWorkflowRig(t)
	acc, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(w.Conv.Revision), Conversation: w.Conv.Ref, Assignee: deskActor})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if acc.Operation.State != model.Committed {
		t.Fatalf("operation %+v, want committed", acc.Operation)
	}
	cur := w.stored(t)
	if cur.Assignee == nil || *cur.Assignee != deskActor || cur.Revision == w.Conv.Revision {
		t.Fatalf("stored %+v, want assigned to %s at a new revision", cur, deskActor)
	}
	evs := w.events(t)
	if len(evs) != 1 || evs[0].Kind != model.EventConversationAssigned || evs[0].Audience.Visibility != model.Internal {
		t.Fatalf("events %+v, want one internal conversation.assigned", evs)
	}
	p := payloadOf[model.ConversationAssignmentPayload](t, evs[0])
	if p.Assignee == nil || *p.Assignee != deskActor || p.Previous != nil || p.PreviousRevision != w.Conv.Revision {
		t.Fatalf("payload %+v", p)
	}

	if _, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: deskActor}); err != nil {
		t.Fatalf("Assign again: %v", err)
	}
	if again := w.stored(t); again.Revision != cur.Revision || len(w.events(t)) != 1 {
		t.Fatalf("re-assigning the assignee moved the revision or emitted: %+v", again)
	}

	_, err = w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(w.Conv.Revision), Conversation: w.Conv.Ref, Assignee: "desk-2"})
	requireCode(t, err, model.Conflict, "")
	if after := w.stored(t); *after.Assignee != deskActor || after.Revision != cur.Revision {
		t.Fatalf("stale assign changed %+v", after)
	}

	reassign := w.cmd(cur.Revision)
	_, err = w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: reassign, Conversation: w.Conv.Ref, Assignee: "desk-2"})
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	evs = w.events(t)
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	if p := payloadOf[model.ConversationAssignmentPayload](t, evs[1]); p.Assignee == nil || *p.Assignee != "desk-2" || p.Previous == nil || *p.Previous != deskActor {
		t.Fatalf("reassign payload %+v, want desk-2 replacing %s", p, deskActor)
	}
	// A retry replays; the same operation ID on Unassign is another
	// payload.
	replay, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: reassign, Conversation: w.Conv.Ref, Assignee: "desk-2"})
	if err != nil || !replay.Replayed {
		t.Fatalf("retry = %+v, %v, want a replay", replay, err)
	}
	_, err = w.desk.Conversations().Unassign(w.Ctx, model.ConversationAssignment{Command: model.Command{Call: call(), Operation: reassign.Operation}, Conversation: w.Conv.Ref})
	requireCode(t, err, model.Conflict, "")
}

// Unassign clears the assignee with an unassigned event naming the one it
// replaced; an unassigned conversation is a no-op.
func TestUnassign(t *testing.T) {
	w := newWorkflowRig(t)
	if _, err := w.desk.Conversations().Unassign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref}); err != nil {
		t.Fatalf("Unassign unassigned: %v", err)
	}
	if cur := w.stored(t); cur.Revision != w.Conv.Revision || len(w.events(t)) != 0 {
		t.Fatalf("no-op unassign changed %+v", cur)
	}
	if _, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: deskActor}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.desk.Conversations().Unassign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref}); err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	if cur := w.stored(t); cur.Assignee != nil {
		t.Fatalf("still assigned: %+v", cur)
	}
	evs := w.events(t)
	if len(evs) != 2 || evs[1].Kind != model.EventConversationUnassigned {
		t.Fatalf("events %+v, want assigned then unassigned", evs)
	}
	if p := payloadOf[model.ConversationAssignmentPayload](t, evs[1]); p.Assignee != nil || p.Previous == nil || *p.Previous != deskActor {
		t.Fatalf("unassigned payload %+v", p)
	}
}

// The request shape decides the call: Assign needs an assignee, Unassign
// refuses one, Snooze needs a future until, Unsnooze refuses one.
func TestWorkflowRequestShapes(t *testing.T) {
	w := newWorkflowRig(t)
	cv := w.desk.Conversations()
	past, future := w.Clock.Now().Add(-time.Minute), w.Clock.Now().Add(time.Hour)
	now := w.Clock.Now()
	_, err := cv.Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref})
	requireCode(t, err, model.Validation, "assignee")
	_, err = cv.Unassign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: deskActor})
	requireCode(t, err, model.Validation, "assignee")
	_, err = cv.Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref})
	requireCode(t, err, model.Validation, "until")
	_, err = cv.Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &past})
	requireCode(t, err, model.Validation, "until")
	_, err = cv.Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &now})
	requireCode(t, err, model.Validation, "until")
	_, err = cv.Unsnooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &future})
	requireCode(t, err, model.Validation, "until")
	foreign := w.Conv.Ref
	foreign.Inbox.Tenant = tenantB
	_, err = cv.Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: foreign, Assignee: deskActor})
	requireCode(t, err, model.NotFound, "")
	if len(w.events(t)) != 0 || w.stored(t).Revision != w.Conv.Revision {
		t.Fatal("a refused request changed the conversation")
	}
}

// An assignee must be eligible: the authorizer is asked whether the
// assignee itself may assign the conversation. A viewer is not, and an
// external participant may neither assign nor snooze.
func TestAssignEligibilityAndCallers(t *testing.T) {
	w := newWorkflowRig(t)
	_, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: viewerActor})
	requireCode(t, err, model.Unauthorized, "")
	var asked bool
	for _, a := range w.pol.asked(model.ActionConversationAssign) {
		if a.Principal.Actor == viewerActor && a.Resource == w.Conv.Ref.Ref() {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("eligibility not asked as the assignee: %+v", w.pol.asked(model.ActionConversationAssign))
	}
	until := w.Clock.Now().Add(time.Hour)
	for _, actor := range []model.ActorID{externalActor, viewerActor} {
		c := clientAs(t, w.rig, actor)
		_, err := c.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: deskActor})
		requireCode(t, err, model.Unauthorized, "")
		_, err = c.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &until})
		requireCode(t, err, model.Unauthorized, "")
	}
	if cur := w.stored(t); cur.Assignee != nil || cur.Snooze != nil || len(w.events(t)) != 0 {
		t.Fatalf("refused calls changed %+v", cur)
	}
}

// A snooze is shown while in effect and wakes on an inbound message
// recorded after it was set: Get drops it and Unsnooze is then a no-op.
func TestSnoozeWakesOnInbound(t *testing.T) {
	w := newWorkflowRig(t)
	w.inbound(t, "00000003-0000-7000-8000-000000000001")
	w.Clock.Advance(time.Second)
	until := w.Clock.Now().Add(time.Hour)
	if _, err := w.desk.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &until}); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	got := w.get(t, w.desk)
	if got.Snooze == nil || !got.Snooze.Until.Equal(until) || got.Snooze.By != deskActor || !got.Snooze.At.Equal(w.Clock.Now()) {
		t.Fatalf("snooze %+v, want until %v by %s set now", got.Snooze, until, deskActor)
	}
	evs := w.events(t)
	if len(evs) != 1 || evs[0].Kind != model.EventConversationSnoozed {
		t.Fatalf("events %+v", evs)
	}
	if p := payloadOf[model.ConversationSnoozePayload](t, evs[0]); p.Until == nil || !p.Until.Equal(until) {
		t.Fatalf("snoozed payload %+v", p)
	}
	w.Clock.Advance(time.Second)
	w.inbound(t, "00000003-0000-7000-8000-000000000002")
	if got := w.get(t, w.desk); got.Snooze != nil {
		t.Fatalf("snooze %+v shown after an inbound message", got.Snooze)
	}
	rev := w.stored(t).Revision
	if _, err := w.desk.Conversations().Unsnooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref}); err != nil {
		t.Fatalf("Unsnooze: %v", err)
	}
	if w.stored(t).Revision != rev || len(w.events(t)) != 1 {
		t.Fatal("unsnoozing a woken conversation changed it")
	}
}

// A snooze expires at Until with no write and no event; Unsnooze ends one
// in effect with an unsnoozed event.
func TestSnoozeExpiryAndUnsnooze(t *testing.T) {
	w := newWorkflowRig(t)
	until := w.Clock.Now().Add(time.Hour)
	if _, err := w.desk.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &until}); err != nil {
		t.Fatal(err)
	}
	rev := w.stored(t).Revision
	w.Clock.Advance(time.Hour)
	if got := w.get(t, w.desk); got.Snooze != nil {
		t.Fatalf("snooze %+v shown at Until", got.Snooze)
	}
	if w.stored(t).Revision != rev || len(w.events(t)) != 1 {
		t.Fatal("expiry wrote or emitted")
	}
	later := w.Clock.Now().Add(time.Hour)
	if _, err := w.desk.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &later}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.desk.Conversations().Unsnooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref}); err != nil {
		t.Fatal(err)
	}
	if cur := w.stored(t); cur.Snooze != nil {
		t.Fatalf("snooze %+v kept after Unsnooze", cur.Snooze)
	}
	evs := w.events(t)
	if len(evs) != 3 || evs[2].Kind != model.EventConversationUnsnoozed {
		t.Fatalf("events %+v, want snoozed, snoozed, unsnoozed", evs)
	}
}

// Workflow state is zeroed for a reader without conversation.read_workflow
// on Get and List, and never exported; a viewer holding it sees it.
func TestWorkflowStateHiddenFromExternal(t *testing.T) {
	w := newWorkflowRig(t)
	until := w.Clock.Now().Add(time.Hour)
	if _, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: deskActor}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.desk.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &until}); err != nil {
		t.Fatal(err)
	}
	ext := clientAs(t, w.rig, externalActor)
	if got := w.get(t, ext); got.Assignee != nil || got.Snooze != nil {
		t.Fatalf("external Get sees %+v", got)
	}
	pg, err := ext.Conversations().List(w.Ctx, model.ListRequest{Call: call(), Page: model.PageRequest{Limit: 10}})
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("external List = %+v, %v", pg, err)
	}
	if got := pg.Items[0]; got.Assignee != nil || got.Snooze != nil {
		t.Fatalf("external List sees %+v", got)
	}
	viewer := clientAs(t, w.rig, viewerActor)
	if got := w.get(t, viewer); got.Assignee == nil || got.Snooze == nil {
		t.Fatalf("viewer with read_workflow sees %+v", got)
	}
	pg, err = w.desk.Conversations().List(w.Ctx, model.ListRequest{Call: call(), Page: model.PageRequest{Limit: 10}})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Assignee == nil || pg.Items[0].Snooze == nil {
		t.Fatalf("desk List = %+v, %v", pg, err)
	}
}

// Workflow events reach only a principal that may read workflow state.
func TestWorkflowEventsNeedReadWorkflow(t *testing.T) {
	w := newWorkflowRig(t)
	if _, err := w.desk.Conversations().Assign(w.Ctx, model.ConversationAssignment{Command: w.cmd(""), Conversation: w.Conv.Ref, Assignee: deskActor}); err != nil {
		t.Fatal(err)
	}
	listed := func(c enginetest.Client) int {
		t.Helper()
		pg, err := c.Events().List(w.Ctx, model.ListEvents{Call: call(), Start: model.StartBeginning, Limit: 100})
		if err != nil {
			t.Fatalf("Events.List: %v", err)
		}
		n := 0
		for _, ev := range pg.Items {
			if model.IsWorkflowEvent(ev.Kind) {
				n++
			}
		}
		return n
	}
	if n := listed(w.desk); n != 1 {
		t.Fatalf("desk lists %d workflow events, want 1", n)
	}
	// A principal that may read the conversation's internal content but
	// not its workflow state receives nothing.
	pol := &internalNoWorkflow{}
	r := newRig(t, withPolicy(pol))
	desk := clientAs(t, r, deskActor)
	if _, err := desk.Conversations().Assign(r.Ctx, model.ConversationAssignment{Command: model.Command{Call: call(), Operation: "op-x"}, Conversation: r.Conv.Ref, Assignee: deskActor}); err != nil {
		t.Fatal(err)
	}
	other := clientAs(t, r, "reader-1")
	pg, err := other.Events().List(r.Ctx, model.ListEvents{Call: call(), Start: model.StartBeginning, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range pg.Items {
		if model.IsWorkflowEvent(ev.Kind) {
			t.Fatalf("reader without read_workflow received %+v", ev)
		}
	}
}

// internalNoWorkflow allows everything except conversation.read_workflow
// to reader-1.
type internalNoWorkflow struct{}

func (internalNoWorkflow) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	return policy.Decision{Allow: a.Principal.Actor != "reader-1" || a.Action != model.ActionConversationReadWorkflow}, nil
}
