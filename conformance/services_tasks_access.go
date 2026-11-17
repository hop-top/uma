package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

// task.act is authorized on the conversation for an opening and on the
// task for an update; a denial writes nothing.
func taskActUnauthorizedActor(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	m := env.message(conv.Ref, model.Public)
	task := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	env.policy.deny(model.ActionTaskAct)
	head := env.eventHead()
	_, err := env.client.Tasks().Act(env.ctx, env.openReq(conv.Ref, m.Ref, ""))
	serviceCode(t, err, model.Unauthorized)
	_, err = env.client.Tasks().Act(env.ctx, env.updateReq(task.Ref, task.Revision, model.TaskUpdate{
		State: model.TaskWorking, StatusMessage: ptrTo(textContent("hidden")),
	}))
	serviceCode(t, err, model.Unauthorized)
	if got := env.getMessage(m.Ref); got.Task != nil {
		t.Fatalf("denied opening bound the message to %v", got.Task)
	}
	if got := env.getTask(task.Ref); got.Revision != task.Revision {
		t.Fatalf("denied update changed the task: %+v", got)
	}
	wantTaskRefs(t, "tasks", env.listTasks(model.Filter{Conversation: &conv.Ref}, 10), task.Ref)
	if evs := env.eventsAfter(head); len(evs) != 0 {
		t.Fatalf("events = %v", eventKinds(evs))
	}
}

// Reads are task.read per task; a reference in another tenant, or naming
// the task under another conversation, is not found.
func taskGetCrossTenantNotFound(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	task := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	get := func(ref model.TaskRef) error {
		_, err := env.client.Tasks().Get(env.ctx, model.Lookup[model.TaskRef]{Call: model.Call{Correlation: "corr"}, Ref: ref})
		return err
	}
	foreign := task.Ref
	foreign.Conversation.Inbox.Tenant = "conformance-other"
	serviceCode(t, get(foreign), model.NotFound)
	_, err := env.client.Tasks().Act(env.ctx, env.updateReq(foreign, task.Revision, model.TaskUpdate{State: model.TaskWorking}))
	serviceCode(t, err, model.NotFound)
	moved := task.Ref
	moved.Conversation = env.conv.Ref
	serviceCode(t, get(moved), model.NotFound)
	serviceCode(t, get(model.TaskRef{Conversation: conv.Ref, ID: env.id()}), model.NotFound)
	_, err = env.client.Messages().List(env.ctx, serviceList(model.Filter{Task: &foreign}, 10, ""))
	serviceCode(t, err, model.NotFound)
	env.policy.denyOn(model.ActionTaskRead, task.Ref.Ref())
	serviceCode(t, get(task.Ref), model.Unauthorized)
}

// Tasks.List pages most recent status first and honours inboxes,
// conversation and task states; it admits per task and rejects any other
// filter field, the owner identity included.
func taskListStatusOrderAndFilters(t *testing.T, env *serviceEnv) {
	conv, other := env.agentConversation(), env.agentConversation()
	a := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	env.clock.Advance(time.Second)
	b := env.openTask(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	env.clock.Advance(time.Second)
	c := env.openTask(other.Ref, env.message(other.Ref, model.Public).Ref, "")
	env.clock.Advance(time.Second)
	a = env.updateTask(a, model.TaskUpdate{State: model.TaskWorking})
	wantTaskRefs(t, "all", env.listTasks(model.Filter{}, 1), a.Ref, c.Ref, b.Ref)
	wantTaskRefs(t, "conversation", env.listTasks(model.Filter{Conversation: &conv.Ref}, 10), a.Ref, b.Ref)
	wantTaskRefs(t, "submitted", env.listTasks(model.Filter{Statuses: []string{model.TaskSubmitted}}, 10), c.Ref, b.Ref)
	wantTaskRefs(t, "inbox2", env.listTasks(model.Filter{Inboxes: []model.InboxRef{env.inbox2}}, 10))
	env.policy.denyOn(model.ActionTaskRead, c.Ref.Ref())
	wantTaskRefs(t, "admitted", env.listTasks(model.Filter{}, 10), a.Ref, b.Ref)
	owner := model.IdentityRef{Inbox: env.inbox, ID: env.id()}
	for name, f := range map[string]model.Filter{
		"identity": {Identity: &owner},
		"state":    {Statuses: []string{model.TaskUnspecified}},
		"task":     {Task: &a.Ref},
		"text":     {Text: "x"},
	} {
		_, err := env.client.Tasks().List(env.ctx, serviceList(f, 10, ""))
		if model.CodeOf(err) != model.Validation {
			t.Fatalf("%s filter: error = %v, want validation", name, err)
		}
	}
}

// The same operation ID and payload returns the stored operation, even
// after the task moved on; an opening replays without a second task.
func taskActReplaySamePayload(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	open := env.openReq(conv.Ref, env.message(conv.Ref, model.Public).Ref, "")
	first, err := env.client.Tasks().Act(env.ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	again, err := env.client.Tasks().Act(env.ctx, open)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref || *again.Operation.ResultTask != *first.Operation.ResultTask {
		t.Fatalf("replayed opening = %+v, %v", again, err)
	}
	wantTaskRefs(t, "tasks", env.listTasks(model.Filter{Conversation: &conv.Ref}, 10), *first.Operation.ResultTask)
	task := env.getTask(*first.Operation.ResultTask)
	upd := env.updateReq(task.Ref, task.Revision, model.TaskUpdate{State: model.TaskWorking, StatusMessage: ptrTo(textContent("on it"))})
	u1, err := env.client.Tasks().Act(env.ctx, upd)
	if err != nil {
		t.Fatal(err)
	}
	env.updateTask(env.getTask(task.Ref), model.TaskUpdate{State: model.TaskCompleted})
	head := env.eventHead()
	u2, err := env.client.Tasks().Act(env.ctx, upd)
	if err != nil || !u2.Replayed || u2.Operation.Ref != u1.Operation.Ref || *u2.Operation.ResultTask != task.Ref {
		t.Fatalf("replayed update = %+v, %v", u2, err)
	}
	if evs := env.eventsAfter(head); len(evs) != 0 {
		t.Fatalf("replay events = %v", eventKinds(evs))
	}
	if got := env.messages(model.Filter{Task: &task.Ref}, 10); len(got) != 2 {
		t.Fatalf("task history = %v, want the opening message and one status message", got)
	}
}

// The same operation ID with another payload is a conflict.
func taskActReplayDifferentPayload(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	m1, m2 := env.message(conv.Ref, model.Public), env.message(conv.Ref, model.Public)
	open := env.openReq(conv.Ref, m1.Ref, "")
	a, err := env.client.Tasks().Act(env.ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	other := env.openReq(conv.Ref, m2.Ref, "")
	other.Operation = open.Operation
	_, err = env.client.Tasks().Act(env.ctx, other)
	serviceCode(t, err, model.Conflict)
	task := env.getTask(*a.Operation.ResultTask)
	upd := env.updateReq(task.Ref, task.Revision, model.TaskUpdate{State: model.TaskWorking})
	if _, err := env.client.Tasks().Act(env.ctx, upd); err != nil {
		t.Fatal(err)
	}
	diff := env.updateReq(task.Ref, task.Revision, model.TaskUpdate{State: model.TaskFailed})
	diff.Operation = upd.Operation
	_, err = env.client.Tasks().Act(env.ctx, diff)
	serviceCode(t, err, model.Conflict)
	if got := env.getTask(task.Ref); got.State != model.TaskWorking {
		t.Fatalf("task = %+v", got)
	}
	if got := env.getMessage(m2.Ref); got.Task != nil {
		t.Fatalf("conflicting opening bound %v", got.Task)
	}
}

// Messages.List by task lists the task's history, admitted per message; a
// reply to a message carrying a task joins it, and records its parent.
func messagesListByTaskAndReplyInheritsTask(t *testing.T, env *serviceEnv) {
	conv := env.agentConversation()
	m := env.message(conv.Ref, model.Public)
	loose := env.message(conv.Ref, model.Public)
	task := env.openTask(conv.Ref, m.Ref, model.TaskWorking)
	task = env.updateTask(task, model.TaskUpdate{
		State: model.TaskInputRequired, StatusMessage: ptrTo(textContent("internal")), Audience: &model.Audience{Visibility: model.Internal},
	})
	reply := func(parent *model.MessageRef) model.Message {
		t.Helper()
		a, err := env.client.Messages().Reply(env.ctx, model.Reply{Command: env.cmd(""), Conversation: conv.Ref, InReplyTo: parent, Content: textContent("answer")})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		return env.getMessage(*a.Operation.ResultMessage)
	}
	r1 := reply(&m.Ref)
	if r1.Task == nil || *r1.Task != task.Ref || r1.InReplyTo == nil || *r1.InReplyTo != m.Ref {
		t.Fatalf("reply = task %v, in reply to %v", r1.Task, r1.InReplyTo)
	}
	r2 := reply(&loose.Ref)
	if r2.Task != nil || r2.InReplyTo == nil || *r2.InReplyTo != loose.Ref {
		t.Fatalf("reply to a loose message = task %v, in reply to %v", r2.Task, r2.InReplyTo)
	}
	// Without a parent the latest inbound message is answered.
	r3 := reply(nil)
	if r3.InReplyTo == nil || *r3.InReplyTo != loose.Ref || r3.Task != nil {
		t.Fatalf("default parent = %v, task %v", r3.InReplyTo, r3.Task)
	}
	byTask := model.Filter{Task: &task.Ref}
	if got := env.messages(byTask, 1); !slices.Equal(got, []model.ID{m.Ref.ID, r1.Ref.ID}) {
		t.Fatalf("member history = %v", got)
	}
	env.policy.setStaff(true)
	if got := env.messages(byTask, 10); !slices.Equal(got, []model.ID{m.Ref.ID, task.StatusMessage.ID, r1.Ref.ID}) {
		t.Fatalf("staff history = %v", got)
	}
	f := byTask
	f.Conversation = &env.conv.Ref
	if got := env.messages(f, 10); len(got) != 0 {
		t.Fatalf("task history under another conversation = %v", got)
	}
}
