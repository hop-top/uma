package conformance

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// serviceTaskCases run Tasks.Get, List and Act, and the task history of
// Messages, through the engine on the store under test.
var serviceTaskCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"TaskOpenFromMessage", taskOpenFromMessage},
	{"TaskOpenOnlyInAgentContexts", taskOpenOnlyInAgentContexts},
	{"TaskUpdateStateAndArtifacts", taskUpdateStateAndArtifacts},
	{"TaskStateOnlyUpdateEmitsNoMessage", taskStateOnlyUpdateEmitsNoMessage},
	{"TaskStatusMessageRecordedOnceWithOrigin", taskStatusMessageRecordedOnce},
	{"TaskTerminalRejectsUpdate", taskTerminalRejectsUpdate},
	{"TaskCancelRequestedThenCanceled", taskCancelRequestedThenCanceled},
	{"TaskActUnauthorizedActor", taskActUnauthorizedActor},
	{"TaskGetCrossTenantNotFound", taskGetCrossTenantNotFound},
	{"TaskListStatusOrderAndFilters", taskListStatusOrderAndFilters},
	{"TaskActReplaySamePayloadReturnsOperation", taskActReplaySamePayload},
	{"TaskActReplayDifferentPayloadConflict", taskActReplayDifferentPayload},
	{"MessagesListByTaskAndReplyInheritsTask", messagesListByTaskAndReplyInheritsTask},
}

// Opening allocates the task, binds the message and records task.updated
// (created) in one committed operation whose ResultTask is the task.
func taskOpenFromMessage(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	m := env.message(conv.Ref, model.Public)
	head := env.eventHead()
	a, err := env.client.Tasks().Act(env.ctx, env.openReq(conv.Ref, m.Ref, ""))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	op := a.Operation
	if op.State != model.Committed || op.ResultTask == nil || op.Result == nil || *op.Result != op.ResultTask.Ref() || !op.ResultTask.In(conv.Ref) {
		t.Fatalf("operation = %+v", op)
	}
	task := env.getTask(*op.ResultTask)
	now := env.clock.Now().UTC()
	if task.State != model.TaskSubmitted || task.Context != conv.ReplyRoute || task.External != model.TaskExternalRef(env.ch, task.Ref.ID) ||
		!task.CreatedAt.Equal(now) || !task.StatusAt.Equal(now) || task.StatusMessage != nil || task.CancelRequested {
		t.Fatalf("task = %+v", task)
	}
	if got := env.getMessage(m.Ref); got.Task == nil || *got.Task != task.Ref {
		t.Fatalf("message task = %v", got.Task)
	}
	evs := env.eventsAfter(head)
	if !slices.Equal(eventKinds(evs), []string{model.EventTaskUpdated}) {
		t.Fatalf("events = %v", eventKinds(evs))
	}
	p := taskPayload(t, evs[0])
	if p.Task != task.Ref || !slices.Equal(p.Changes, []string{model.TaskFieldCreated}) || p.Trigger != model.TriggerCommand ||
		p.State == nil || *p.State != (model.StatusChange{To: model.TaskSubmitted}) || p.Message == nil || *p.Message != m.Ref ||
		p.Operation == nil || *p.Operation != op.Ref || p.PreviousRevision != "" || evs[0].Revision != task.Revision {
		t.Fatalf("payload = %+v", p)
	}
	if w := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, model.TaskWorking); w.State != model.TaskWorking {
		t.Fatalf("opened working = %+v", w)
	}
}

// Tasks exist only in per-agent-context conversations; a message is bound
// to one task at most; an opening's message lies in its conversation.
func taskOpenOnlyInAgentContexts(t *testing.T, env *serviceEnv) {
	m := env.message(env.conv.Ref, model.Public)
	_, err := env.client.Tasks().Act(env.ctx, env.openReq(env.conv.Ref, m.Ref, ""))
	serviceCode(t, err, model.Validation)
	// A context reference alone does not make an agent conversation.
	room := env.agentConversation()
	room.Mode, room.Revision = model.ModePerRoom, "cv2"
	env.put(func(tx store.Tx) error { return tx.PutConversation(env.ctx, room, "cv1") })
	_, err = env.client.Tasks().Act(env.ctx, env.openReq(room.Ref, env.message(room.Ref, model.Public).Ref, ""))
	serviceCode(t, err, model.Validation)
	conv := env.agentConversation()
	m2 := env.message(conv.Ref, model.Public)
	_, err = env.client.Tasks().Act(env.ctx, env.openReq(conv.Ref, m.Ref, ""))
	serviceCode(t, err, model.Validation)
	env.openTask(conv.Ref, m2.Ref, "")
	_, err = env.client.Tasks().Act(env.ctx, env.openReq(conv.Ref, m2.Ref, ""))
	serviceCode(t, err, model.Conflict)
	missing := model.MessageRef{Conversation: conv.Ref, ID: env.id()}
	_, err = env.client.Tasks().Act(env.ctx, env.openReq(conv.Ref, missing, ""))
	serviceCode(t, err, model.NotFound)
	named := env.openReq(conv.Ref, m2.Ref, "")
	named.Target.ID = env.id()
	_, err = env.client.Tasks().Act(env.ctx, named)
	serviceCode(t, err, model.Validation)
	// Role or author fields are no part of an action: the actor is always
	// the caller.
	role := env.openReq(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	role.Action.Data = json.RawMessage(`{"message":` + string(taskData(t, m2.Ref)) + `,"role":"agent"}`)
	_, err = env.client.Tasks().Act(env.ctx, role)
	serviceCode(t, err, model.Validation)
	wantTaskRefs(t, "tasks", env.listTasks(model.Filter{Conversation: &env.conv.Ref}, 10))
}

// An update moves the state along CanTransition, upserts artifacts by ID
// and reports exactly the fields that changed.
func taskUpdateStateAndArtifacts(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	task := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	env.clock.Advance(time.Minute)
	head := env.eventHead()
	next := env.updateTask(task, model.TaskUpdate{State: model.TaskWorking, Artifacts: []model.ArtifactUpdate{
		{ID: "report", Name: "Report", Content: textContent("part 1")},
	}})
	if next.State != model.TaskWorking || next.Revision == task.Revision || !next.StatusAt.Equal(env.clock.Now().UTC()) ||
		!next.CreatedAt.Equal(task.CreatedAt) || len(next.Artifacts) != 1 || next.Artifacts[0].Name != "Report" {
		t.Fatalf("task = %+v", next)
	}
	evs := env.eventsAfter(head)
	p := taskPayload(t, evs[len(evs)-1])
	if len(evs) != 1 || !slices.Equal(p.Changes, []string{model.TaskFieldState, model.TaskFieldArtifacts}) ||
		*p.State != (model.StatusChange{From: model.TaskSubmitted, To: model.TaskWorking}) || p.PreviousRevision != task.Revision || p.Message != nil {
		t.Fatalf("events = %v, payload = %+v", eventKinds(evs), p)
	}
	next = env.updateTask(next, model.TaskUpdate{Artifacts: []model.ArtifactUpdate{
		{ID: "report", Content: textContent("part 2"), Append: true, Complete: true},
	}})
	a := next.Artifacts[0]
	if next.State != model.TaskWorking || a.Name != "Report" || !a.Complete || len(a.Content.Parts) != 2 || a.Content.Parts[1].Text != "part 2" {
		t.Fatalf("appended = %+v", next)
	}
	// Interrupted resumes; submitted is never re-entered.
	next = env.updateTask(next, model.TaskUpdate{State: model.TaskInputRequired})
	next = env.updateTask(next, model.TaskUpdate{State: model.TaskWorking})
	_, err := env.client.Tasks().Act(env.ctx, env.updateReq(next.Ref, next.Revision, model.TaskUpdate{State: model.TaskSubmitted}))
	serviceCode(t, err, model.Conflict)
	_, err = env.client.Tasks().Act(env.ctx, env.updateReq(next.Ref, task.Revision, model.TaskUpdate{State: model.TaskCompleted}))
	serviceCode(t, err, model.Conflict)
	// A same-state update that changes nothing commits without event.
	head = env.eventHead()
	if same := env.updateTask(next, model.TaskUpdate{State: model.TaskWorking}); same.Revision != next.Revision {
		t.Fatalf("no-op changed the revision: %s -> %s", next.Revision, same.Revision)
	}
	if evs := env.eventsAfter(head); len(evs) != 0 {
		t.Fatalf("no-op events = %v", eventKinds(evs))
	}
}

// A state change without content updates the task and emits task.updated
// only: no message is synthesized.
func taskStateOnlyUpdateEmitsNoMessage(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	m := env.message(conv.Ref, model.Public)
	task := env.openTask(conv.Ref, m.Ref, "")
	head := env.eventHead()
	for _, s := range []string{model.TaskWorking, model.TaskInputRequired, model.TaskWorking, model.TaskCompleted} {
		task = env.updateTask(task, model.TaskUpdate{State: s})
	}
	if task.StatusMessage != nil {
		t.Fatalf("status message = %v", task.StatusMessage)
	}
	if got := env.messages(model.Filter{Conversation: &conv.Ref}, 10); !slices.Equal(got, []model.ID{m.Ref.ID}) {
		t.Fatalf("messages = %v", got)
	}
	evs := env.eventsAfter(head)
	if !slices.Equal(eventKinds(evs), slices.Repeat([]string{model.EventTaskUpdated}, 4)) {
		t.Fatalf("events = %v", eventKinds(evs))
	}
}

// Status content is recorded as exactly one uma-authored message bound to
// the task, under the action's audience, with message.created (origin
// task_status) and task.updated naming it; never message.received.
func taskStatusMessageRecordedOnce(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	m := env.message(conv.Ref, model.Public)
	task := env.openTask(conv.Ref, m.Ref, model.TaskWorking)
	env.clock.Advance(time.Minute)
	head := env.eventHead()
	audience := model.Audience{Visibility: model.Restricted, PolicyRef: "agents"}
	req := env.updateReq(task.Ref, task.Revision, model.TaskUpdate{
		State: model.TaskInputRequired, StatusMessage: ptrTo(textContent("Please provide the document")), Audience: &audience,
	})
	a, err := env.client.Tasks().Act(env.ctx, req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	next := env.getTask(task.Ref)
	env.policy.setStaff(true)
	msgs := env.messages(model.Filter{Conversation: &conv.Ref}, 10)
	if len(msgs) != 2 || msgs[0] != m.Ref.ID || next.StatusMessage == nil || next.StatusMessage.ID != msgs[1] {
		t.Fatalf("messages = %v, status message = %v", msgs, next.StatusMessage)
	}
	sm := env.getMessage(*next.StatusMessage)
	if sm.Task == nil || *sm.Task != task.Ref || sm.Audience != audience || sm.Source != serviceType || sm.InReplyTo != nil ||
		sm.Origin == nil || sm.Origin.Actor != "staff-1" || sm.Origin.Operation != a.Operation.Ref || sm.Content.Parts[0].Text != "Please provide the document" {
		t.Fatalf("status message = %+v", sm)
	}
	if !next.StatusAt.Equal(env.clock.Now().UTC()) || next.State != model.TaskInputRequired {
		t.Fatalf("task = %+v", next)
	}
	evs := env.eventsAfter(head)
	if !slices.Equal(eventKinds(evs), []string{model.EventMessageCreated, model.EventTaskUpdated}) {
		t.Fatalf("events = %v", eventKinds(evs))
	}
	c := createdPayload(t, evs[0])
	if c.Message != sm.Ref || c.Origin != model.MessageOriginTaskStatus || c.Trigger != model.TriggerCommand ||
		c.Operation != a.Operation.Ref || c.Source != serviceType || evs[0].Audience != audience {
		t.Fatalf("message.created = %+v, audience %v", c, evs[0].Audience)
	}
	p := taskPayload(t, evs[1])
	if p.Message == nil || *p.Message != sm.Ref || !slices.Equal(p.Changes, []string{model.TaskFieldState, model.TaskFieldStatusMessage}) {
		t.Fatalf("task.updated = %+v", p)
	}
	// Without an audience the status message is public; a same-state
	// status update records one more message and changes no state.
	next = env.updateTask(next, model.TaskUpdate{StatusMessage: ptrTo(textContent("Still waiting"))})
	if next.State != model.TaskInputRequired || env.getMessage(*next.StatusMessage).Audience != (model.Audience{Visibility: model.Public}) {
		t.Fatalf("task = %+v", next)
	}
	if got := env.messages(model.Filter{Conversation: &conv.Ref}, 10); len(got) != 3 {
		t.Fatalf("messages = %v", got)
	}
}

// A terminal task is final: every update is a conflict.
func taskTerminalRejectsUpdate(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	for _, s := range []string{model.TaskCompleted, model.TaskFailed, model.TaskCanceled, model.TaskRejected} {
		task := env.updateTask(env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, model.TaskWorking), model.TaskUpdate{State: s})
		head := env.eventHead()
		for _, u := range []model.TaskUpdate{
			{State: model.TaskWorking},
			{State: s},
			{StatusMessage: ptrTo(textContent("late"))},
			{Artifacts: []model.ArtifactUpdate{{ID: "a", Content: textContent("late")}}},
		} {
			_, err := env.client.Tasks().Act(env.ctx, env.updateReq(task.Ref, task.Revision, u))
			serviceCode(t, err, model.Conflict)
		}
		if got := env.getTask(task.Ref); got.Revision != task.Revision || got.State != s {
			t.Fatalf("terminal task changed: %+v", got)
		}
		if evs := env.eventsAfter(head); len(evs) != 0 {
			t.Fatalf("events = %v", eventKinds(evs))
		}
	}
}

// A pending cancel request is cleared when the host reports canceled.
func taskCancelRequestedThenCanceled(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	task := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, model.TaskWorking)
	task.CancelRequested, task.Revision = true, "cancel-1"
	env.put(func(tx store.Tx) error { return tx.PutTask(env.ctx, task, env.getTask(task.Ref).Revision) })
	head := env.eventHead()
	next := env.updateTask(task, model.TaskUpdate{State: model.TaskCanceled})
	if next.State != model.TaskCanceled || next.CancelRequested {
		t.Fatalf("task = %+v", next)
	}
	p := taskPayload(t, env.eventsAfter(head)[0])
	if !slices.Equal(p.Changes, []string{model.TaskFieldState, model.TaskFieldCancelRequested}) {
		t.Fatalf("changes = %v", p.Changes)
	}
}
