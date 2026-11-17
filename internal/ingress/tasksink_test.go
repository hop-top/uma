package ingress_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// denyActions allows everything except the actions it was told to deny.
type denyActions struct {
	mu     sync.Mutex
	denied map[string]bool
}

func (p *denyActions) deny(actions ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denied = map[string]bool{}
	for _, a := range actions {
		p.denied[a] = true
	}
}

func (p *denyActions) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return policy.Decision{Allow: !p.denied[a.Action]}, nil
}

// sinkWorld is a per-agent-context inbox holding, for alice, tasks a1 and
// a2 in context c1 (a continuation message after a1's opening) and a3 in
// c2, and for bob, b1 in cb. Each message is recorded one second after the
// previous.
type sinkWorld struct {
	*harness
	policy         *denyActions
	ts             adapter.TaskSink
	in             model.Inbox
	a1, a2, a3, b1 model.Task
}

func newSinkWorld(t *testing.T) *sinkWorld {
	t.Helper()
	p := &denyActions{}
	h := newHarness(t, chatType, func(c *engine.Config) { c.Authorizer = p })
	w := &sinkWorld{harness: h, policy: p, ts: h.Sink, in: agentInbox(h)}
	open := func(event, from, ctxID string) model.Task {
		h.Clock.Advance(time.Second)
		h.MustAccept(agentMessage(h, event, from, ctxID, openTask()))
		m := h.MessageByProviderID(w.in.Ref, "m-"+event)
		return w.task(*m.Task)
	}
	w.a1 = open("e1", "alice", "c1")
	h.Clock.Advance(time.Second)
	h.MustAccept(agentMessage(h, "e2", "alice", "c1", continuation(h, w.a1.Ref)))
	w.a2 = open("e3", "alice", "c1")
	w.a3 = open("e4", "alice", "c2")
	w.b1 = open("e5", "bob", "cb")
	return w
}

func (w *sinkWorld) task(ref model.TaskRef) model.Task {
	w.T.Helper()
	var t model.Task
	w.View(func(r store.Reader) error {
		var err error
		t, err = r.GetTask(w.Ctx, ref)
		return err
	})
	return t
}

func (w *sinkWorld) alice() model.ExternalRef { return w.SenderRef("alice") }
func (w *sinkWorld) bob() model.ExternalRef   { return w.SenderRef("bob") }

func (w *sinkWorld) ext(t model.Task) model.ExternalRef { return t.External }

// host reports progress on t as the host would, one second later.
func (w *sinkWorld) host(t model.Task, u model.TaskUpdate) {
	w.T.Helper()
	w.Clock.Advance(time.Second)
	data, err := json.Marshal(u)
	if err != nil {
		w.T.Fatal(err)
	}
	cur := w.task(t.Ref)
	_, err = w.Client.Tasks().Act(w.Ctx, model.TaskAction{
		Command: model.Command{Call: model.Call{Correlation: "host"}, Operation: nextOp(), ExpectedRevision: cur.Revision},
		Target:  t.Ref,
		Action:  model.Extension{Name: model.SchemaTaskUpdate, Version: model.PayloadVersion1, Data: data},
	})
	if err != nil {
		w.T.Fatalf("Tasks.Act: %v", err)
	}
}

var (
	opMu  sync.Mutex
	opSeq int
)

func nextOp() model.OperationID {
	opMu.Lock()
	defer opMu.Unlock()
	opSeq++
	return model.OperationID(fmt.Sprintf("sink-op-%d", opSeq))
}

func ids(ts []model.Task) []model.ID {
	out := make([]model.ID, len(ts))
	for i, t := range ts {
		out[i] = t.Ref.ID
	}
	return out
}

// A principal reaches the contexts and tasks it owns; a context or task of
// another principal, an unknown one and an unclaimed principal are the
// same model.NotFound.
func TestTaskSinkContextAndTask(t *testing.T) {
	w := newSinkWorld(t)
	c1 := contextRef(w.harness, "c1")
	conv, err := w.ts.Context(w.Ctx, w.alice(), c1)
	if err != nil || conv.Ref != w.a1.Ref.Conversation || conv.Mode != model.ModePerAgentContext {
		t.Fatalf("Context = %+v, %v; want a1's conversation", conv, err)
	}
	got, err := w.ts.Task(w.Ctx, w.alice(), w.ext(w.a1))
	if err != nil || got.Ref != w.a1.Ref || got.State != model.TaskSubmitted {
		t.Fatalf("Task = %+v, %v; want a1", got, err)
	}
	var want string
	for name, call := range map[string]func() error{
		"context of another principal": func() error { _, err := w.ts.Context(w.Ctx, w.bob(), c1); return err },
		"unknown context":              func() error { _, err := w.ts.Context(w.Ctx, w.alice(), contextRef(w.harness, "nope")); return err },
		"unclaimed principal":          func() error { _, err := w.ts.Context(w.Ctx, w.SenderRef("carol"), c1); return err },
		"task of another principal":    func() error { _, err := w.ts.Task(w.Ctx, w.bob(), w.ext(w.a1)); return err },
		"unknown task": func() error {
			_, err := w.ts.Task(w.Ctx, w.alice(), model.TaskExternalRef(w.Channel.Ref, "no-such-task"))
			return err
		},
	} {
		err := call()
		requireCode(t, err, model.NotFound, "")
		if msg := err.(*model.Error).SafeMessage; want == "" {
			want = msg
		} else if msg != want {
			t.Fatalf("%s: message %q differs from %q", name, msg, want)
		}
	}
	// Malformed arguments.
	_, err = w.ts.Context(w.Ctx, w.alice(), w.Ref(model.RefKindRoom, "c1", ""))
	requireCode(t, err, model.Validation, "")
	_, err = w.ts.Task(w.Ctx, w.alice(), c1)
	requireCode(t, err, model.Validation, "")
	foreign := w.alice()
	foreign.Namespace = "com.example.undeclared"
	_, err = w.ts.Task(w.Ctx, foreign, w.ext(w.a1))
	requireCode(t, err, model.Validation, "namespace")
	other := w.alice()
	other.Channel = w.PutChannel(chatType).Ref
	_, err = w.ts.Task(w.Ctx, other, w.ext(w.a1))
	requireCode(t, err, model.Validation, "bound channel")
}

// Tasks pages the principal's own tasks, most recent status first,
// narrowed by context and state.
func TestTaskSinkTasks(t *testing.T) {
	w := newSinkWorld(t)
	page, err := w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{})
	if err != nil || !page.Complete {
		t.Fatalf("Tasks = %+v, %v", page, err)
	}
	if want := []model.ID{w.a3.Ref.ID, w.a2.Ref.ID, w.a1.Ref.ID}; !slices.Equal(ids(page.Items), want) {
		t.Fatalf("alice's tasks %v, want %v", ids(page.Items), want)
	}
	if page, _ := w.ts.Tasks(w.Ctx, w.bob(), adapter.TaskQuery{}); !slices.Equal(ids(page.Items), []model.ID{w.b1.Ref.ID}) {
		t.Fatalf("bob's tasks %v, want only b1", ids(page.Items))
	}
	c1 := contextRef(w.harness, "c1")
	page, err = w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{Context: &c1})
	if err != nil || !slices.Equal(ids(page.Items), []model.ID{w.a2.Ref.ID, w.a1.Ref.ID}) {
		t.Fatalf("c1 tasks %v, %v", ids(page.Items), err)
	}
	w.host(w.a1, model.TaskUpdate{State: model.TaskWorking})
	page, _ = w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{States: []string{model.TaskWorking}})
	if !slices.Equal(ids(page.Items), []model.ID{w.a1.Ref.ID}) {
		t.Fatalf("working tasks %v, want a1", ids(page.Items))
	}
	// The most recent status first: a1 moved up.
	page, _ = w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{Page: model.PageRequest{Limit: 2}})
	if page.Complete || len(page.Items) != 2 || page.Items[0].Ref != w.a1.Ref {
		t.Fatalf("first page %v complete %v, want a1 first and more", ids(page.Items), page.Complete)
	}
	rest, _ := w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{Page: model.PageRequest{Limit: 2, After: page.Next}})
	if !rest.Complete || len(rest.Items) != 1 {
		t.Fatalf("second page %v complete %v, want the last task", ids(rest.Items), rest.Complete)
	}
	// Another principal's context lists nothing, as an absent one does.
	cb := contextRef(w.harness, "cb")
	page, err = w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{Context: &cb})
	if err != nil || len(page.Items) != 0 || !page.Complete {
		t.Fatalf("foreign context = %+v, %v; want an empty page", page, err)
	}
	// An unclaimed principal has no tasks.
	if page, err := w.ts.Tasks(w.Ctx, w.SenderRef("carol"), adapter.TaskQuery{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("unclaimed principal = %+v, %v", page, err)
	}
	_, err = w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{States: []string{"bogus"}})
	requireCode(t, err, model.Validation, "")
}

// A task's message history holds the messages bound to it in recording
// order: the opening and continuing messages and the status messages the
// host recorded.
func TestTaskSinkTaskMessages(t *testing.T) {
	w := newSinkWorld(t)
	status := model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "on it"}}}
	w.host(w.a1, model.TaskUpdate{State: model.TaskWorking, StatusMessage: &status})
	page, err := w.ts.TaskMessages(w.Ctx, w.alice(), w.a1.Ref, model.PageRequest{})
	if err != nil || !page.Complete || len(page.Items) != 3 {
		t.Fatalf("history = %+v, %v; want 3 messages", page, err)
	}
	first := w.MessageByProviderID(w.in.Ref, "m-e1")
	second := w.MessageByProviderID(w.in.Ref, "m-e2")
	if page.Items[0].Ref != first.Ref || page.Items[1].Ref != second.Ref || page.Items[2].Origin == nil {
		t.Fatalf("history order %+v", page.Items)
	}
	one, _ := w.ts.TaskMessages(w.Ctx, w.alice(), w.a1.Ref, model.PageRequest{Limit: 1})
	if len(one.Items) != 1 || one.Complete || one.Items[0].Ref != first.Ref {
		t.Fatalf("limited page %+v", one)
	}
	_, err = w.ts.TaskMessages(w.Ctx, w.bob(), w.a1.Ref, model.PageRequest{})
	requireCode(t, err, model.NotFound, "")
	// Another task's history is its own.
	if p, _ := w.ts.TaskMessages(w.Ctx, w.alice(), w.a2.Ref, model.PageRequest{}); len(p.Items) != 1 {
		t.Fatalf("a2 history = %d messages, want its opening", len(p.Items))
	}
}

// reply seeds a uma-authored reply to parent, as Messages.Reply records one.
func (w *sinkWorld) reply(parent model.MessageRef, id string, task *model.TaskRef) model.Message {
	w.T.Helper()
	w.Clock.Advance(time.Second)
	p := parent
	m := model.Message{
		Ref:      model.MessageRef{Conversation: parent.Conversation, ID: model.ID(id)},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "reply " + id}}},
		Audience: model.Audience{Visibility: model.Public}, Task: task, InReplyTo: &p,
		OccurredAt: w.Clock.Now(), RecordedAt: w.Clock.Now(), Revision: "r1", Source: chatType,
		Origin: &model.Origin{Actor: "host", Operation: model.OperationRef{Tenant: tenantA, ID: model.ID(id)}},
	}
	w.Tx(func(tx store.Tx) error { return tx.PutMessage(w.Ctx, m, "") })
	return m
}

// Responses answers a message with its uma-authored replies and, only once
// the task changed after the message (or the message opened it), the task.
func TestTaskSinkResponses(t *testing.T) {
	w := newSinkWorld(t)
	opener := w.MessageByProviderID(w.in.Ref, "m-e1")
	cont := w.MessageByProviderID(w.in.Ref, "m-e2")
	// Opening message: the task answers at once.
	r, err := w.ts.Responses(w.Ctx, w.alice(), opener.Ref)
	if err != nil || r.Task == nil || r.Task.Ref != w.a1.Ref || len(r.Replies) != 0 {
		t.Fatalf("opener = %+v, %v; want a1 and no replies", r, err)
	}
	// A continuation is not answered by the task's earlier state.
	r, err = w.ts.Responses(w.Ctx, w.alice(), cont.Ref)
	if err != nil || r.Task != nil {
		t.Fatalf("continuation = %+v, %v; want no task yet", r, err)
	}
	w.host(w.a1, model.TaskUpdate{State: model.TaskWorking})
	r, err = w.ts.Responses(w.Ctx, w.alice(), cont.Ref)
	if err != nil || r.Task == nil || r.Task.State != model.TaskWorking {
		t.Fatalf("continuation after a change = %+v, %v; want the working task", r, err)
	}
	// Replies answer their parent only, in recording order.
	r1 := w.reply(cont.Ref, "00000000-0000-7000-8000-0000000000a1", &w.a1.Ref)
	r2 := w.reply(cont.Ref, "00000000-0000-7000-8000-0000000000a2", nil)
	w.reply(opener.Ref, "00000000-0000-7000-8000-0000000000a3", nil)
	// An inbound message replying to it is not a uma-authored reply.
	inbound := agentMessage(w.harness, "e8", "alice", "c1", nil)
	inbound.InReplyTo = "m-e2"
	w.MustAccept(inbound)
	if got := w.MessageByProviderID(w.in.Ref, "m-e8"); got.InReplyTo == nil || *got.InReplyTo != cont.Ref {
		t.Fatalf("inbound reply link = %v, want %v", got.InReplyTo, cont.Ref)
	}
	r, err = w.ts.Responses(w.Ctx, w.alice(), cont.Ref)
	if err != nil || len(r.Replies) != 2 || r.Replies[0].Ref != r1.Ref || r.Replies[1].Ref != r2.Ref {
		t.Fatalf("replies = %+v, %v; want the two replies to the continuation", r.Replies, err)
	}
	// A message-only exchange has no task.
	w.MustAccept(agentMessage(w.harness, "e9", "alice", "c1", nil))
	only := w.MessageByProviderID(w.in.Ref, "m-e9")
	w.reply(only.Ref, "00000000-0000-7000-8000-0000000000a4", nil)
	r, err = w.ts.Responses(w.Ctx, w.alice(), only.Ref)
	if err != nil || r.Task != nil || len(r.Replies) != 1 {
		t.Fatalf("message-only = %+v, %v; want one reply and no task", r, err)
	}
	_, err = w.ts.Responses(w.Ctx, w.bob(), opener.Ref)
	requireCode(t, err, model.NotFound, "")
	_, err = w.ts.Responses(w.Ctx, w.alice(), model.MessageRef{Conversation: opener.Ref.Conversation, ID: "00000000-0000-7000-8000-0000000000ff"})
	requireCode(t, err, model.NotFound, "")
}

func (w *sinkWorld) cancel(p model.ExternalRef, task model.Task, op model.OperationID) (model.Task, error) {
	return w.ts.CancelTask(w.Ctx, p, task.Ref, model.Command{Call: model.Call{Correlation: "cancel"}, Operation: op})
}

// A cancel request marks the task and emits task.updated; the host decides
// the state. A repeat changes nothing, a terminal task refuses it, and
// another principal's task is not found.
func TestTaskSinkCancelRequested(t *testing.T) {
	w := newSinkWorld(t)
	before := len(w.Events())
	got, err := w.cancel(w.alice(), w.a1, "cancel-1")
	if err != nil || !got.CancelRequested || got.State != model.TaskSubmitted || got.Revision == w.a1.Revision {
		t.Fatalf("CancelTask = %+v, %v; want a requested, still submitted task", got, err)
	}
	if stored := w.task(w.a1.Ref); stored.CancelRequested != true || stored.Revision != got.Revision || !stored.StatusAt.Equal(w.a1.StatusAt) {
		t.Fatalf("stored task %+v", stored)
	}
	evs := w.Events()[before:]
	if len(evs) != 1 || evs[0].Kind != model.EventTaskUpdated || evs[0].Actor != serviceAct || evs[0].Resource != w.a1.Ref.Ref() {
		t.Fatalf("events %v, want one task.updated by the service principal", eventKinds(evs))
	}
	ups := taskUpdates(t, evs)
	if len(ups) != 1 || !slices.Equal(ups[0].Changes, []string{model.TaskFieldCancelRequested}) || ups[0].Trigger != model.TriggerCommand ||
		ups[0].Operation == nil || ups[0].State != nil || ups[0].PreviousRevision != w.a1.Revision {
		t.Fatalf("payload %+v, want cancel_requested by command under an operation", ups)
	}
	// The same command replays; a new one on a requested task is a no-op.
	for _, op := range []model.OperationID{"cancel-1", "cancel-2"} {
		again, err := w.cancel(w.alice(), w.a1, op)
		if err != nil || !again.CancelRequested || again.Revision != got.Revision {
			t.Fatalf("repeat %s = %+v, %v; want the task unchanged", op, again, err)
		}
	}
	if n := len(w.Events()) - before; n != 1 {
		t.Fatalf("events after repeats = %d, want 1", n)
	}
	// The host answers: canceled clears the request.
	w.host(w.a1, model.TaskUpdate{State: model.TaskCanceled})
	if stored := w.task(w.a1.Ref); stored.CancelRequested || stored.State != model.TaskCanceled {
		t.Fatalf("after the host canceled: %+v", stored)
	}
	_, err = w.cancel(w.alice(), w.a1, "cancel-3")
	requireCode(t, err, model.Conflict, "canceled")
	_, err = w.cancel(w.bob(), w.a2, "cancel-4")
	requireCode(t, err, model.NotFound, "")
	if w.task(w.a2.Ref).CancelRequested {
		t.Fatal("another principal's cancel marked the task")
	}
	_, err = w.ts.CancelTask(w.Ctx, w.alice(), w.a2.Ref, model.Command{Call: model.Call{Correlation: "c"}})
	requireCode(t, err, model.Validation, "")
	_, err = w.ts.CancelTask(w.Ctx, w.alice(), w.a2.Ref, model.Command{Call: model.Call{Correlation: "c"}, Operation: "x", ExpectedRevision: "r1"})
	requireCode(t, err, model.Validation, "expected_revision")
}

// The sink reads and acts as its service principal: a denial is
// model.Unauthorized and nothing is returned or written.
func TestTaskSinkDeniedRead(t *testing.T) {
	w := newSinkWorld(t)
	opener := w.MessageByProviderID(w.in.Ref, "m-e1")
	c1 := contextRef(w.harness, "c1")

	w.policy.deny(model.ActionTaskRead)
	_, err := w.ts.Task(w.Ctx, w.alice(), w.ext(w.a1))
	requireCode(t, err, model.Unauthorized, "")
	_, err = w.ts.TaskMessages(w.Ctx, w.alice(), w.a1.Ref, model.PageRequest{})
	requireCode(t, err, model.Unauthorized, "")
	_, err = w.ts.Responses(w.Ctx, w.alice(), opener.Ref)
	requireCode(t, err, model.Unauthorized, "")
	if page, err := w.ts.Tasks(w.Ctx, w.alice(), adapter.TaskQuery{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("Tasks under a denial = %+v, %v; want none", page, err)
	}

	w.policy.deny(model.ActionConversationRead)
	_, err = w.ts.Context(w.Ctx, w.alice(), c1)
	requireCode(t, err, model.Unauthorized, "")

	w.policy.deny(model.ActionMessageRead)
	_, err = w.ts.Responses(w.Ctx, w.alice(), opener.Ref)
	requireCode(t, err, model.Unauthorized, "")
	if page, err := w.ts.TaskMessages(w.Ctx, w.alice(), w.a1.Ref, model.PageRequest{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("TaskMessages under a message denial = %+v, %v; want none", page, err)
	}

	w.policy.deny(model.ActionTaskAct)
	before := len(w.Events())
	_, err = w.cancel(w.alice(), w.a1, "cancel-denied")
	requireCode(t, err, model.Unauthorized, "")
	if w.task(w.a1.Ref).CancelRequested || len(w.Events()) != before {
		t.Fatal("a denied cancel wrote")
	}

	// Reads that were not denied still work, and a denial does not hide
	// other principals' absence: not found stays not found.
	w.policy.deny()
	if _, err := w.ts.Task(w.Ctx, w.alice(), w.ext(w.a1)); err != nil {
		t.Fatalf("Task after the grant: %v", err)
	}
}

// The sink implements the engine-side half of the task sink for the bound
// channel only.
func TestTaskSinkSeesBoundChannelOnly(t *testing.T) {
	w := newSinkWorld(t)
	other := w.PutChannel(chatType)
	ref := w.alice()
	ref.Channel = other.Ref
	_, err := w.ts.Tasks(w.Ctx, ref, adapter.TaskQuery{})
	requireCode(t, err, model.Validation, "")
}
