package conformance

import (
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// agentTaskCases run an agent protocol through the engine on the store
// under test: remote agents (principals of the fake agent adapter) send
// messages into per-agent-context inboxes, and the host, the only
// executor, opens tasks, replies and reports progress through the public
// services. They check the rules a protocol adapter relies on: contexts
// and tasks, retries, cancellation and the separation of the two roles.
var agentTaskCases = []struct {
	name string
	run  func(*testing.T, *agentEnv)
}{
	{"MessageOnlyExchangeCreatesNoTask", agentMessageOnly},
	{"MultiTaskPerContextIndependentTerminal", agentMultiTask},
	{"InitialRequestAllocatesContextAndConversationAtomically", agentInitialRequestAtomic},
	{"RetryRecoversSameContext", agentRetryRecoversContext},
	{"WaiterGoneTaskStillInspectable", agentWaiterGone},
	{"MismatchedContextTaskRejected", agentMismatchedContextTask},
	{"UnauthorizedTaskPairRejected", agentUnauthorizedTaskPair},
	{"TerminalTaskRejectsContinuation", agentTerminalRejectsContinuation},
	{"CancellationRoundTrip", agentCancellationRoundTrip},
	{"StatusOnlyUpdateEmitsNoMessage", agentStatusOnly},
	{"StatusWithMessageEmitsOne", agentStatusWithMessage},
	{"RoleSeparation", agentRoleSeparation},
	{"SenderKindFixedByFirstClaim", agentSenderKindFirstClaim},
	{"RedactedStatusMessageKeepsTaskRef", agentRedactedStatusMessage},
}

// A message that opens no task leaves none: the exchange is the message and
// its replies, and only a reply to that message answers it.
func agentMessageOnly(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	r := env.mustSend(a, "m-1", sendOpts{})
	other := env.mustSend(a, "m-2", sendOpts{context: r.Context.ID})
	if r.Task != nil || other.Task != nil {
		t.Fatalf("receipts carry tasks %v, %v; a message-only exchange opens none", r.Task, other.Task)
	}
	msg := env.message(*r.Message)
	if msg.Task != nil || msg.Author.Kind != model.ContactKindAgent || msg.Author.External != a || msg.Author.Assurance != "verified" {
		t.Fatalf("message = %+v", msg)
	}
	if got := env.allTasks(); len(got) != 0 {
		t.Fatalf("tasks = %v", taskRefs(got))
	}
	if got := eventKinds(env.events()); slices.Contains(got, model.EventTaskUpdated) {
		t.Fatalf("events = %v, want no task.updated", got)
	}
	if resp, out := env.await(a, *r.Message, 20*time.Millisecond); !out || resp.Task != nil || len(resp.Replies) != 0 {
		t.Fatalf("responses before any reply = %+v (timed out %v)", resp, out)
	}
	env.clock.Advance(time.Second)
	reply := env.hostReply(*other.Message, "about the second")
	if resp, _ := env.await(a, *r.Message, 20*time.Millisecond); len(resp.Replies) != 0 {
		t.Fatalf("a reply to the second message answers the first: %+v", resp)
	}
	reply2 := env.hostReply(*r.Message, "about the first")
	resp, out := env.await(a, *r.Message, time.Second)
	if out || resp.Task != nil || len(resp.Replies) != 1 || resp.Replies[0].Ref != reply2.Ref {
		t.Fatalf("responses = %+v, want only the reply to the message", resp)
	}
	if reply2.InReplyTo == nil || *reply2.InReplyTo != *r.Message || reply.InReplyTo == nil || *reply.InReplyTo != *other.Message {
		t.Fatalf("replies record their parents %v, %v", reply.InReplyTo, reply2.InReplyTo)
	}
	if got := env.allTasks(); len(got) != 0 {
		t.Fatalf("tasks after replies = %v", taskRefs(got))
	}
}

// Several tasks live in one context. Each reaches its own terminal state;
// completing one leaves the others and the conversation untouched, and a
// message continues only a task that is still open.
func agentMultiTask(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	r1 := env.mustSend(a, "m-1", sendOpts{open: true})
	r2 := env.mustSend(a, "m-2", sendOpts{open: true, context: r1.Context.ID})
	if r1.Task == nil || r2.Task == nil || *r1.Conversation != *r2.Conversation || r1.Task.ID == r2.Task.ID {
		t.Fatalf("receipts %+v, %+v: want two tasks of one conversation", r1, r2)
	}
	conv := *r1.Conversation
	env.clock.Advance(time.Second)
	env.hostUpdate(*r1.Task, model.TaskUpdate{State: model.TaskCompleted})
	b := env.hostTask(*r2.Task)
	if b.State != model.TaskSubmitted || env.conversation(conv).Status != model.ConversationOpen {
		t.Fatalf("task B %s, conversation %s after completing task A", b.State, env.conversation(conv).Status)
	}
	env.clock.Advance(time.Second)
	env.hostUpdate(*r2.Task, model.TaskUpdate{State: model.TaskWorking})
	resp, _ := env.await(a, *r2.Message, 0)
	if resp.Task == nil || resp.Task.Ref != *r2.Task || resp.Task.State != model.TaskWorking {
		t.Fatalf("responses of the second message = %+v", resp.Task)
	}
	// The open task continues; the finished one does not, whichever way
	// the continuation arrives.
	r3 := env.mustSend(a, "m-3", sendOpts{task: r2.Task.ID})
	if r3.Task == nil || *r3.Task != *r2.Task || *r3.Conversation != conv {
		t.Fatalf("continuation receipt = %+v", r3)
	}
	if m := env.message(*r3.Message); m.Task == nil || *m.Task != *r2.Task {
		t.Fatalf("continuing message task = %v", m.Task)
	}
	before := env.state()
	_, err := env.send(a, "m-4", sendOpts{task: r1.Task.ID})
	serviceCode(t, err, model.Conflict)
	_, err = env.accept(env.envelope(a, "m-5", model.ExternalRef{}, sendOpts{task: r1.Task.ID}))
	serviceCode(t, err, model.Conflict)
	env.wantState(before, "a continuation of a completed task")
	env.hostUpdate(*r2.Task, model.TaskUpdate{State: model.TaskFailed})
	if got := env.hostTask(*r1.Task); got.State != model.TaskCompleted {
		t.Fatalf("task A = %s after task B failed", got.State)
	}
	if c := env.conversation(conv); c.Status != model.ConversationOpen {
		t.Fatalf("conversation = %s, want it open: no task ends a conversation", c.Status)
	}
	got, err := env.host.Tasks().List(env.ctx, serviceList(model.Filter{Conversation: &conv}, 10, ""))
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("Tasks.List = %+v, %v", got.Items, err)
	}
}

// An initial request carries no context, so each delivery of it brings a
// context of its own. Sixteen at once leave one context, one conversation,
// one message and one task; the other fifteen are duplicates carrying the
// first acceptance's references.
func agentInitialRequestAtomic(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	const n = 16
	type result struct {
		r   model.IngressReceipt
		err error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			r, err := env.accept(env.envelope(a, "m-1", env.freshContext(), sendOpts{open: true}))
			results[i] = result{r, err}
		})
	}
	close(start)
	wg.Wait()
	var first *model.IngressReceipt
	dups := 0
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("accept %d: %v", i, res.err)
		}
		if res.r.Duplicate {
			dups++
		} else {
			first = &results[i].r
		}
	}
	if dups != n-1 || first == nil || !first.Committed {
		t.Fatalf("%d duplicates of %d, first %+v; want one commit and %d duplicates", dups, n, first, n-1)
	}
	for i, res := range results {
		r := res.r
		if r.Conversation == nil || r.Context == nil || r.Message == nil || r.Task == nil ||
			*r.Conversation != *first.Conversation || *r.Context != *first.Context || *r.Message != *first.Message || *r.Task != *first.Task {
			t.Fatalf("receipt %d = %+v, want the first acceptance's references %+v", i, r, first)
		}
	}
	convs := env.conversations()
	if len(convs) != 1 || convs[0].Ref != *first.Conversation || len(convs[0].External) != 1 || convs[0].External[0] != *first.Context ||
		convs[0].ReplyRoute != *first.Context || convs[0].Owner == nil {
		t.Fatalf("conversations = %+v, want the one of %+v", convs, first)
	}
	if got := len(env.allMessages()); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	if got := taskRefs(env.allTasks()); !slices.Equal(got, []model.TaskRef{*first.Task}) {
		t.Fatalf("tasks = %v, want only %v", got, *first.Task)
	}
	if got := eventKinds(env.events(model.EventMessageReceived, model.EventTaskUpdated)); len(got) != 2 {
		t.Fatalf("events = %v, want one message.received and one task.updated", got)
	}
	// Many requests of one context race for its one conversation too.
	ctx := first.Context.ID
	var into sync.WaitGroup
	again := make(chan struct{})
	errs := make([]error, n)
	for i := range n {
		into.Go(func() {
			<-again
			_, errs[i] = env.send(a, "c-"+itoa(i), sendOpts{context: ctx})
		})
	}
	close(again)
	into.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("send %d into the context: %v", i, err)
		}
	}
	if got := len(env.conversations()); got != 1 {
		t.Fatalf("conversations = %d after %d requests into one context, want 1", got, n)
	}
	if got := len(env.allMessages()); got != n+1 {
		t.Fatalf("messages = %d, want %d", got, n+1)
	}
}

// A retry of an accepted message, carrying a context of its own again,
// recovers the first acceptance: the same conversation, context, message
// and task, and nothing recorded twice. The retry's own context, flags
// and sender-chosen id of another agent change nothing.
func agentRetryRecoversContext(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	first := env.mustSend(a, "m-1", sendOpts{open: true})
	before := env.state()
	for _, o := range []sendOpts{{open: true}, {}, {text: "same id, other text"}} {
		r, err := env.send(a, "m-1", o)
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if !r.Duplicate || r.Conversation == nil || r.Context == nil || r.Message == nil || r.Task == nil ||
			*r.Conversation != *first.Conversation || *r.Context != *first.Context || *r.Message != *first.Message || *r.Task != *first.Task {
			t.Fatalf("retry %+v = %+v, want a duplicate of %+v", o, r, first)
		}
	}
	env.wantState(before, "retries")
	// The id is the agent's own: another agent's message of the same id is
	// a message of its own, in a context of its own.
	b := env.principal("agent-2")
	other := env.mustSend(b, "m-1", sendOpts{})
	if *other.Conversation == *first.Conversation || other.Context.ID == first.Context.ID {
		t.Fatalf("agent 2's m-1 = %+v, want its own conversation", other)
	}
}

// The side that was waiting for an answer may be long gone when the host
// answers. The task opened and completed then is there to inspect, with
// its events; nothing was cancelled on the way.
func agentWaiterGone(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	r := env.mustSend(a, "m-1", sendOpts{})
	if resp, out := env.await(a, *r.Message, 30*time.Millisecond); !out || resp.Task != nil {
		t.Fatalf("responses = %+v (timed out %v), want none yet", resp, out)
	}
	if got := len(env.allTasks()); got != 0 {
		t.Fatalf("waiting opened %d tasks", got)
	}
	env.clock.Advance(time.Minute)
	task := env.hostOpen(*r.Message, model.TaskWorking)
	env.clock.Advance(time.Minute)
	done := env.hostUpdate(task.Ref, model.TaskUpdate{
		State: model.TaskCompleted, Artifacts: []model.ArtifactUpdate{{ID: "out", Content: textContent("result")}},
	})
	resp, out := env.await(a, *r.Message, time.Second)
	if out || resp.Task == nil || resp.Task.Ref != task.Ref || resp.Task.State != model.TaskCompleted {
		t.Fatalf("responses = %+v (timed out %v), want the completed task", resp.Task, out)
	}
	got, err := env.tasks.Task(env.ctx, a, env.taskRef(task.Ref.ID))
	if err != nil || got.State != model.TaskCompleted || got.CancelRequested || len(got.Artifacts) != 1 || got.Revision != done.Revision {
		t.Fatalf("sink task = %+v, %v", got, err)
	}
	if hostGot := env.hostTask(task.Ref); hostGot.Revision != done.Revision {
		t.Fatalf("Tasks.Get = %+v", hostGot)
	}
	var states []string
	for _, ev := range env.events(model.EventTaskUpdated) {
		p := taskPayload(t, ev)
		if slices.Contains(p.Changes, model.TaskFieldCancelRequested) || p.State != nil && p.State.To == model.TaskCanceled {
			t.Fatalf("task.updated %+v: nothing was cancelled", p)
		}
		if p.State != nil {
			states = append(states, p.State.To)
		}
	}
	if !slices.Equal(states, []string{model.TaskWorking, model.TaskCompleted}) {
		t.Fatalf("task states %v, want the opening and the completion", states)
	}
}

// A task continues only in its own context: a continuation naming another
// context is refused by the engine and by the adapter's own check, and
// leaves nothing behind. Without a context the same message is accepted.
func agentMismatchedContextTask(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	one := env.mustSend(a, "m-1", sendOpts{open: true})
	two := env.mustSend(a, "m-2", sendOpts{open: true})
	before := env.state()
	_, err := env.accept(env.envelope(a, "m-3", *two.Context, sendOpts{task: one.Task.ID}))
	serviceCode(t, err, model.Conflict)
	_, err = env.send(a, "m-3", sendOpts{task: one.Task.ID, context: two.Context.ID})
	serviceCode(t, err, model.Validation)
	env.wantState(before, "a continuation under another context")
	r := env.mustSend(a, "m-3", sendOpts{task: one.Task.ID, context: one.Context.ID})
	if *r.Conversation != *one.Conversation || r.Task == nil || *r.Task != *one.Task {
		t.Fatalf("continuation in its own context = %+v", r)
	}
	// The message the refused attempt would have been is still new.
	if r.Duplicate {
		t.Fatal("a refused message was recorded as accepted")
	}
}

// A task is reachable by the agent that owns its context and by no other:
// not by id, not through its context, not to cancel, read or continue. A
// task of another inbox is not found by a continuation either, nor is a
// reference into another tenant. Every refusal is the same not-found as for
// a task that does not exist, and writes nothing.
func agentUnauthorizedTaskPair(t *testing.T, env *agentEnv) {
	a, b := env.principal("agent-1"), env.principal("agent-2")
	ra := env.mustSend(a, "m-1", sendOpts{open: true})
	env.mustSend(b, "m-b", sendOpts{open: true})
	env.clock.Advance(time.Second)
	env.hostUpdate(*ra.Task, model.TaskUpdate{State: model.TaskWorking})
	before := env.state()
	taskExt := env.taskRef(ra.Task.ID)
	_, err := env.tasks.Task(env.ctx, b, taskExt)
	serviceCode(t, err, model.NotFound)
	_, err = env.tasks.Context(env.ctx, b, *ra.Context)
	serviceCode(t, err, model.NotFound)
	_, err = env.tasks.TaskMessages(env.ctx, b, *ra.Task, model.PageRequest{})
	serviceCode(t, err, model.NotFound)
	_, err = env.tasks.Responses(env.ctx, b, *ra.Message)
	serviceCode(t, err, model.NotFound)
	_, err = env.tasks.CancelTask(env.ctx, b, *ra.Task, env.cmd(""))
	serviceCode(t, err, model.NotFound)
	page, err := env.tasks.Tasks(env.ctx, b, adapter.TaskQuery{Context: ra.Context})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("another agent lists %v, %v; want nothing", taskRefs(page.Items), err)
	}
	_, err = env.send(b, "m-x", sendOpts{task: ra.Task.ID})
	serviceCode(t, err, model.NotFound)
	_, err = env.send(b, "m-y", sendOpts{context: ra.Context.ID})
	serviceCode(t, err, model.NotFound)
	// The same refusals for what does not exist.
	_, err = env.send(a, "m-z", sendOpts{task: "no-such-task"})
	serviceCode(t, err, model.NotFound)
	_, err = env.send(a, "m-z", sendOpts{context: "no-such-context"})
	serviceCode(t, err, model.NotFound)
	env.wantState(before, "refusals of reads, cancels and continuations")
	if got := env.hostTask(*ra.Task); got.CancelRequested || got.State != model.TaskWorking {
		t.Fatalf("task after the refusals = %+v", got)
	}
	// Another inbox: the owner's task of destination two is no task of
	// destination one.
	two := env.mustSend(a, "m-2", sendOpts{open: true, dest: agentDest2})
	if two.Conversation.Inbox != env.inbox2 {
		t.Fatalf("destination two receipt = %+v", two)
	}
	before = env.state()
	cross := env.envelope(a, "m-3", model.ExternalRef{}, sendOpts{task: two.Task.ID})
	_, err = env.accept(cross)
	serviceCode(t, err, model.NotFound)
	// Another tenant: the reference is outside the sink's tenant.
	foreignTask := *ra.Task
	foreignTask.Conversation.Inbox.Tenant = "conformance-other"
	foreignMsg := *ra.Message
	foreignMsg.Conversation.Inbox.Tenant = "conformance-other"
	_, err = env.tasks.CancelTask(env.ctx, a, foreignTask, env.cmd(""))
	serviceCode(t, err, model.NotFound)
	_, err = env.tasks.TaskMessages(env.ctx, a, foreignTask, model.PageRequest{})
	serviceCode(t, err, model.NotFound)
	_, err = env.tasks.Responses(env.ctx, a, foreignMsg)
	serviceCode(t, err, model.NotFound)
	env.wantState(before, "refused references")
}

// A task that reached any terminal state takes no more messages.
func agentTerminalRejectsContinuation(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	for i, state := range []string{model.TaskCompleted, model.TaskFailed, model.TaskCanceled, model.TaskRejected} {
		r := env.mustSend(a, "open-"+state, sendOpts{open: true})
		env.clock.Advance(time.Second)
		env.hostUpdate(*r.Task, model.TaskUpdate{State: state})
		before := env.state()
		_, err := env.send(a, "late-"+state, sendOpts{task: r.Task.ID})
		serviceCode(t, err, model.Conflict)
		_, err = env.accept(env.envelope(a, "later-"+state, model.ExternalRef{}, sendOpts{task: r.Task.ID}))
		serviceCode(t, err, model.Conflict)
		env.wantState(before, "a continuation of a "+state+" task")
		if got := env.hostTask(*r.Task); got.State != state {
			t.Fatalf("task %d = %s, want %s", i, got.State, state)
		}
	}
}

// A remote cancel request is a flag on the task and an event; the task
// stays where it is until the host reports it canceled, which clears the
// flag. Repeating the request changes nothing, and it ends with the task.
func agentCancellationRoundTrip(t *testing.T, env *agentEnv) {
	a := env.principal("agent-1")
	r := env.mustSend(a, "m-1", sendOpts{open: true})
	env.clock.Advance(time.Second)
	env.hostUpdate(*r.Task, model.TaskUpdate{State: model.TaskWorking})
	cmd := env.cmd("")
	got, err := env.tasks.CancelTask(env.ctx, a, *r.Task, cmd)
	if err != nil || !got.CancelRequested || got.State != model.TaskWorking {
		t.Fatalf("CancelTask = %+v, %v; want the working task with the request recorded", got, err)
	}
	updates := env.events(model.EventTaskUpdated)
	last := taskPayload(t, updates[len(updates)-1])
	if !slices.Equal(last.Changes, []string{model.TaskFieldCancelRequested}) || last.Trigger != model.TriggerCommand ||
		updates[len(updates)-1].Actor != agentService {
		t.Fatalf("cancel event = %+v by %s", last, updates[len(updates)-1].Actor)
	}
	if !env.hostTask(*r.Task).CancelRequested {
		t.Fatal("the host does not see the request")
	}
	before := env.state()
	for _, c := range []model.Command{cmd, env.cmd("")} { // a replay, then a repeated request
		again, err := env.tasks.CancelTask(env.ctx, a, *r.Task, c)
		if err != nil || again.Revision != got.Revision || !again.CancelRequested {
			t.Fatalf("repeated CancelTask = %+v, %v", again, err)
		}
	}
	env.wantState(before, "a repeated cancel request")
	env.clock.Advance(time.Second)
	end := env.hostUpdate(*r.Task, model.TaskUpdate{State: model.TaskCanceled})
	if end.State != model.TaskCanceled || end.CancelRequested {
		t.Fatalf("task after the host canceled it = %+v", end)
	}
	_, err = env.tasks.CancelTask(env.ctx, a, *r.Task, env.cmd(""))
	serviceCode(t, err, model.Conflict)
	resp, out := env.await(a, *r.Message, time.Second)
	if out || resp.Task == nil || resp.Task.State != model.TaskCanceled {
		t.Fatalf("responses = %+v (timed out %v), want the canceled task, not an error", resp.Task, out)
	}
}
