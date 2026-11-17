package ingress_test

import (
	"encoding/json"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// agentInbox configures a per-agent-context inbox on the harness channel.
func agentInbox(h *harness) model.Inbox {
	h.T.Helper()
	return h.Configure(model.Selection{Mode: model.ModePerAgentContext}, dest)
}

func contextRef(h *harness, id string) model.ExternalRef {
	return h.Ref(model.RefKindContext, id, "")
}

// agentMessage is an inbound message from sender into agent context ctxID.
func agentMessage(h *harness, event, from, ctxID string, bind *model.TaskBinding) model.Envelope {
	env := h.Inbound(event, "m-"+event, from, dest, contextRef(h, ctxID))
	env.Task = bind
	return env
}

func openTask() *model.TaskBinding { return &model.TaskBinding{Open: true} }

func continuation(h *harness, task model.TaskRef) *model.TaskBinding {
	return &model.TaskBinding{External: model.TaskExternalRef(h.Channel.Ref, task.ID)}
}

func tasksOf(h *harness, in model.InboxRef) []model.Task {
	return all(h, store.Reader.Tasks, model.Filter{Inboxes: []model.InboxRef{in}})
}

func taskUpdates(t *testing.T, evs []model.Event) []model.TaskUpdatedPayload {
	t.Helper()
	var out []model.TaskUpdatedPayload
	for _, ev := range evs {
		if ev.Kind != model.EventTaskUpdated {
			continue
		}
		var p model.TaskUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// A per-agent-context inbox groups conversations by context reference only:
// two contexts are two conversations, and two senders in one context share
// one. The creating sender owns the conversation, and the context reference
// is its external reference and reply route.
func TestPerAgentContextSelection(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	h.MustAccept(agentMessage(h, "e1", "alice", "c1", nil))
	h.MustAccept(agentMessage(h, "e2", "alice", "c2", nil))
	h.MustAccept(agentMessage(h, "e3", "bob", "c1", nil))
	convs := h.Conversations(in.Ref)
	if len(convs) != 2 {
		t.Fatalf("conversations = %d, want 2 (one per context)", len(convs))
	}
	alice, _ := identityOf(h, in.Ref, "alice")
	byCtx := map[string]model.Conversation{}
	for _, c := range convs {
		if c.Mode != model.ModePerAgentContext || len(c.External) != 1 || c.ReplyRoute != c.External[0] || c.ReplyRoute.Kind != model.RefKindContext {
			t.Fatalf("conversation %+v: want the context reference as external and reply route", c)
		}
		if c.Owner == nil || *c.Owner != alice.Ref {
			t.Fatalf("owner = %v, want the creating principal %v", c.Owner, alice.Ref)
		}
		byCtx[c.External[0].ID] = c
	}
	if n := len(byCtx["c1"].Participants); n != 2 {
		t.Fatalf("c1 participants = %d, want alice and bob", n)
	}
	if n := len(h.Messages(in.Ref)); n != 3 {
		t.Fatalf("messages = %d, want 3", n)
	}
}

// A message needs a context reference unless it continues a task, and a
// rejection leaves no trace.
func TestIngressWithoutContextRejected(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	before := len(h.Events())
	for name, env := range map[string]model.Envelope{
		"no context":   h.Inbound("e1", "m1", "alice", dest),
		"no reference": func() model.Envelope { e := h.Inbound("e2", "m2", "alice", dest); e.Task = openTask(); return e }(),
		"other room":   h.Inbound("e3", "m3", "alice", dest, h.Ref(model.RefKindRoom, "r1", "")),
		"two contexts": h.Inbound("e4", "m4", "alice", dest, contextRef(h, "c1"), contextRef(h, "c2")),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.Accept(env)
			requireCode(t, err, model.Validation, "")
		})
	}
	if len(h.Conversations(in.Ref)) != 0 || len(h.Identities(in.Ref)) != 0 || len(h.Events()) != before {
		t.Fatal("rejected ingress left state")
	}
	// A sender is required: the creating principal owns the context.
	sys := h.Inbound("e5", "m5", "alice", dest, contextRef(h, "c1"))
	sys.Sender, sys.SystemActor = model.ExternalRef{}, true
	_, err := h.Accept(sys)
	requireCode(t, err, model.Validation, "requires a sender")
}

// An opening binding allocates the task in the same unit as the message
// that opened it: the message carries the task, and task.updated (created,
// provider-triggered) joins message.received.
func TestIngressOpensTaskWithMessage(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	r := h.MustAccept(agentMessage(h, "e1", "alice", "c1", openTask()))
	msgs, tasks := h.Messages(in.Ref), tasksOf(h, in.Ref)
	if len(msgs) != 1 || len(tasks) != 1 {
		t.Fatalf("messages %d tasks %d, want 1 and 1", len(msgs), len(tasks))
	}
	msg, task := msgs[0], tasks[0]
	if msg.Task == nil || *msg.Task != task.Ref {
		t.Fatalf("message task = %v, want %v", msg.Task, task.Ref)
	}
	ctx := contextRef(h, "c1")
	if task.State != model.TaskSubmitted || task.Context != ctx || task.External != model.TaskExternalRef(h.Channel.Ref, task.Ref.ID) ||
		task.CancelRequested || !task.CreatedAt.Equal(t0) || !task.StatusAt.Equal(t0) || task.Revision == "" {
		t.Fatalf("task %+v, want submitted in %v with its core reference", task, ctx)
	}
	if err := task.Validate(); err != nil {
		t.Fatalf("stored task invalid: %v", err)
	}
	if r.Task == nil || *r.Task != task.Ref {
		t.Fatalf("receipt task = %v, want %v", r.Task, task.Ref)
	}
	var updated []model.Event
	for _, ev := range h.Events() {
		if ev.Kind == model.EventTaskUpdated {
			updated = append(updated, ev)
		}
	}
	if len(updated) != 1 || updated[0].Resource != task.Ref.Ref() || updated[0].Revision != task.Revision {
		t.Fatalf("task.updated events %+v, want one on the task at its revision", updated)
	}
	want := model.TaskUpdatedPayload{
		Task: task.Ref, Changes: []string{model.TaskFieldCreated}, Trigger: model.TriggerProvider,
		State: &model.StatusChange{To: model.TaskSubmitted}, Message: &msg.Ref,
	}
	if got := taskUpdates(t, h.Events()); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("payload %+v, want %+v", got, want)
	}
	kinds := map[string]int{}
	for _, k := range eventKinds(h.Events()) {
		kinds[k]++
	}
	if kinds[model.EventMessageReceived] != 1 || kinds[model.EventConversationChanged] != 1 || kinds[model.EventTaskUpdated] != 1 || len(kinds) != 3 {
		t.Fatalf("event kinds %v, want conversation.changed, message.received and task.updated once each", kinds)
	}
	// A message without a binding stays a message-only exchange.
	h.MustAccept(agentMessage(h, "e2", "alice", "c1", nil))
	if len(tasksOf(h, in.Ref)) != 1 {
		t.Fatal("a message without a binding opened a task")
	}
}

// The receipt carries what the ingress produced, and a duplicate returns
// the same references.
func TestIngressReceiptCarriesCorrelation(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	env := agentMessage(h, "e1", "alice", "c1", openTask())
	first := h.MustAccept(env)
	conv, msg, task := h.Conversations(in.Ref)[0], h.Messages(in.Ref)[0], tasksOf(h, in.Ref)[0]
	ctx := contextRef(h, "c1")
	if first.Conversation == nil || *first.Conversation != conv.Ref || first.Message == nil || *first.Message != msg.Ref ||
		first.Task == nil || *first.Task != task.Ref || first.Context == nil || *first.Context != ctx {
		t.Fatalf("receipt %+v, want conversation %v message %v task %v context %v", first, conv.Ref, msg.Ref, task.Ref, ctx)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("receipt invalid: %v", err)
	}
	dup := h.MustAccept(env)
	if !dup.Duplicate || dup.ID != first.ID || !reflect.DeepEqual(dup.Conversation, first.Conversation) || !reflect.DeepEqual(dup.Message, first.Message) ||
		!reflect.DeepEqual(dup.Task, first.Task) || !reflect.DeepEqual(dup.Context, first.Context) {
		t.Fatalf("duplicate receipt %+v, want first's references %+v", dup, first)
	}
	// A message-only request has no task reference.
	r := h.MustAccept(agentMessage(h, "e2", "alice", "c1", nil))
	if r.Task != nil || r.Message == nil || r.Conversation == nil || *r.Conversation != conv.Ref {
		t.Fatalf("message-only receipt %+v, want conversation and message, no task", r)
	}
}

// A retry carries a freshly allocated context but the same message: the
// duplicate returns the first receipt and creates nothing.
func TestRetryWithFreshContextRecoversFirst(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	first := h.MustAccept(agentMessage(h, "e1", "alice", "ctx-first", openTask()))
	retry := agentMessage(h, "e1", "alice", "ctx-fresh", openTask())
	dup := h.MustAccept(retry)
	if !dup.Duplicate || dup.Context == nil || dup.Context.ID != "ctx-first" || !reflect.DeepEqual(dup.Conversation, first.Conversation) {
		t.Fatalf("retry receipt %+v, want the first context and conversation", dup)
	}
	if n := len(h.Conversations(in.Ref)); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	if len(tasksOf(h, in.Ref)) != 1 || len(h.Messages(in.Ref)) != 1 {
		t.Fatal("retry created a task or message")
	}
}

// A continuation names an open task; without a context reference it takes
// the task's conversation. It never changes the task or emits task.updated.
func TestIngressContinuationWithoutContext(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	h.MustAccept(agentMessage(h, "e1", "alice", "c1", openTask()))
	task := tasksOf(h, in.Ref)[0]
	cont := h.Inbound("e2", "m-e2", "alice", dest)
	cont.Task = continuation(h, task.Ref)
	r := h.MustAccept(cont)
	if r.Task == nil || *r.Task != task.Ref || r.Context == nil || *r.Context != contextRef(h, "c1") {
		t.Fatalf("receipt %+v, want the task and its context", r)
	}
	if n := len(h.Conversations(in.Ref)); n != 1 {
		t.Fatalf("conversations = %d, want the task's", n)
	}
	m := h.MessageByProviderID(in.Ref, "m-e2")
	if m.Task == nil || *m.Task != task.Ref || m.Ref.Conversation != task.Ref.Conversation {
		t.Fatalf("continuation message %+v, want bound to %v", m, task.Ref)
	}
	if got := tasksOf(h, in.Ref); len(got) != 1 || !reflect.DeepEqual(got[0], task) {
		t.Fatalf("task changed: %+v, was %+v", got, task)
	}
	if n := len(taskUpdates(t, h.Events())); n != 1 {
		t.Fatalf("task.updated events = %d, want only the opening", n)
	}
	// The same context reference may accompany the continuation.
	withCtx := agentMessage(h, "e3", "alice", "c1", continuation(h, task.Ref))
	if r := h.MustAccept(withCtx); r.Task == nil || *r.Task != task.Ref {
		t.Fatalf("receipt %+v, want the task", r)
	}
}

// A continuation whose context is not the task's, or whose task is unknown
// or terminal, is rejected with nothing written.
func TestIngressContinuationMismatch(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	h.MustAccept(agentMessage(h, "e1", "alice", "c1", openTask()))
	task := tasksOf(h, in.Ref)[0]
	before, events := len(h.Messages(in.Ref)), len(h.Events())
	_, err := h.Accept(agentMessage(h, "e2", "alice", "c2", continuation(h, task.Ref)))
	requireCode(t, err, model.Conflict, "context")
	unknown := model.TaskExternalRef(h.Channel.Ref, "no-such-task")
	_, err = h.Accept(agentMessage(h, "e3", "alice", "c1", &model.TaskBinding{External: unknown}))
	requireCode(t, err, model.NotFound, "")
	if len(h.Messages(in.Ref)) != before || len(h.Events()) != events || len(h.Conversations(in.Ref)) != 1 {
		t.Fatal("rejected continuation left state")
	}
}

func TestIngressContinuationTerminal(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	h.MustAccept(agentMessage(h, "e1", "alice", "c1", openTask()))
	task := tasksOf(h, in.Ref)[0]
	done := task.Clone()
	done.State, done.Revision = model.TaskCompleted, "r-done"
	h.Tx(func(tx store.Tx) error { return tx.PutTask(h.Ctx, done, task.Revision) })
	before := len(h.Messages(in.Ref))
	_, err := h.Accept(agentMessage(h, "e2", "alice", "c1", continuation(h, task.Ref)))
	requireCode(t, err, model.Conflict, "completed")
	if len(h.Messages(in.Ref)) != before {
		t.Fatal("message recorded for a terminal task")
	}
}

// Task bindings belong to per-agent-context inboxes and to messages.
func TestIngressTaskBindingOutsideMode(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), "contacts")
	env := h.Inbound("e1", "m1", "u1", "contacts")
	env.Task = openTask()
	_, err := h.Accept(env)
	requireCode(t, err, model.Validation, "per-agent-context")
	agentInbox(h)
	bad := agentMessage(h, "e2", "alice", "c1", &model.TaskBinding{})
	_, err = h.Accept(bad)
	requireCode(t, err, model.Validation, "task")
	edit := h.Inbound("e3", "m3", "alice", dest)
	edit.Kind, edit.Task = model.EnvelopeMessageEdited, openTask()
	_, err = h.Accept(edit)
	requireCode(t, err, model.Validation, "task")
}

// Concurrent initial requests carrying one message converge on one
// conversation, one message and one task; the rest are duplicates.
func TestConcurrentInitialRequestsOneConversationOneTask(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	receipts := make(chan model.IngressReceipt, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := h.Accept(agentMessage(h, "e1", "alice", "c1", openTask()))
			receipts <- r
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}
	dups := 0
	var first *model.IngressReceipt
	for r := range receipts {
		if r.Duplicate {
			dups++
		}
		if first == nil {
			first = &r
		}
		if r.Task == nil || !reflect.DeepEqual(r.Task, first.Task) || !reflect.DeepEqual(r.Conversation, first.Conversation) {
			t.Fatalf("receipt %+v differs from %+v", r, *first)
		}
	}
	if dups != n-1 {
		t.Fatalf("duplicates = %d, want %d", dups, n-1)
	}
	if c, m, k := len(h.Conversations(in.Ref)), len(h.Messages(in.Ref)), len(tasksOf(h, in.Ref)); c != 1 || m != 1 || k != 1 {
		t.Fatalf("conversations %d messages %d tasks %d, want 1 each", c, m, k)
	}
}

// Concurrent first messages of distinct requests into one context converge
// on one conversation.
func TestConcurrentSameContextOneConversation(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.Accept(agentMessage(h, "e"+strconv.Itoa(i), "alice", "c1", nil))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}
	if c, m := len(h.Conversations(in.Ref)), len(h.Messages(in.Ref)); c != 1 || m != n {
		t.Fatalf("conversations %d messages %d, want 1 and %d", c, m, n)
	}
}

// An agent sender is recorded at its first claim, as everywhere.
func TestAgentContextSenderKind(t *testing.T) {
	h := newHarness(t, chatType)
	in := agentInbox(h)
	env := agentMessage(h, "e1", "agent-1", "c1", openTask())
	env.SenderKind = model.ContactKindAgent
	h.MustAccept(env)
	if id, c := identityOf(h, in.Ref, "agent-1"); id.Kind != model.ContactKindAgent || c.Kind != model.ContactKindAgent {
		t.Fatalf("identity kind %q contact kind %q, want agent", id.Kind, c.Kind)
	}
	if p := receivedPayloads(t, h.Events()); len(p) != 1 || p[0].AuthorKind != model.ContactKindAgent {
		t.Fatalf("payloads %+v, want author_kind agent", p)
	}
}
