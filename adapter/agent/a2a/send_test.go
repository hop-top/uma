package a2a_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A host reply inside the window is a Message response: message-only, no
// task, in the context the exchange was allocated.
func TestSendMessageMessageOnly(t *testing.T) {
	r := newRig(t)
	done := async(func() reply { return r.send("agent-1", "m-1", "hello", nil) })
	conv, msgs := r.waitMessages("", 1)
	r.hostReply(conv.Ref, msgs[0], "hi back")
	res := decode[sendResult](t, await(t, done).ok(t))
	if res.Task != nil || res.Message == nil {
		t.Fatalf("result = %+v, want a message and no task", res)
	}
	m := res.Message
	if m.Role != "ROLE_AGENT" || textOf(m.Parts) != "hi back" || m.TaskID != "" || m.MessageID == "" {
		t.Fatalf("message = %+v", m)
	}
	if m.ContextID != conv.External[0].ID || !strings.HasPrefix(m.ContextID, "ctx-") {
		t.Fatalf("context = %q, want the allocated %q", m.ContextID, conv.External[0].ID)
	}
	if n := r.taskCount(); n != 0 {
		t.Fatalf("%d tasks, want none for a message-only exchange", n)
	}
}

func TestSendMessageReturnImmediatelyOpensTask(t *testing.T) {
	r := newRig(t)
	res := decode[sendResult](t, r.send("agent-1", "m-1", "hello", immediately()).ok(t))
	if res.Message != nil || res.Task == nil {
		t.Fatalf("result = %+v, want a task", res)
	}
	if res.Task.Status.State != "TASK_STATE_SUBMITTED" || res.Task.ID == "" || res.Task.ContextID == "" {
		t.Fatalf("task = %+v", res.Task)
	}
	if r.taskCount() != 1 {
		t.Fatalf("%d tasks, want 1", r.taskCount())
	}
	if ts := res.Task.Status.Timestamp; ts != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("timestamp = %q, want the RFC 3339 UTC millisecond form", ts)
	}
	// The task is the one GetTask serves.
	got := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": res.Task.ID}).ok(t))
	if got.ID != res.Task.ID || got.ContextID != res.Task.ContextID {
		t.Fatalf("GetTask = %+v, want %+v", got, res.Task)
	}
}

// Every sender is created as an agent: the first claim fixes the kind.
func TestSenderIsAgent(t *testing.T) {
	r := newRig(t)
	r.send("agent-1", "m-1", "hello", immediately()).ok(t)
	err := r.st.View(t.Context(), tenant, func(rd store.Reader) error {
		id, err := rd.Identity(t.Context(), model.IdentityKey{Inbox: inbox, Namespace: a2a.NamespacePrincipal, SourceID: "agent-1"})
		if err != nil {
			return err
		}
		if id.Kind != model.ContactKindAgent {
			t.Errorf("identity kind = %q, want agent", id.Kind)
		}
		c, err := rd.GetContact(t.Context(), id.Contact)
		if err != nil {
			return err
		}
		if c.Kind != model.ContactKindAgent {
			t.Errorf("contact kind = %q, want agent", c.Kind)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSendMessageBlockingTaskTerminal(t *testing.T) {
	r := newRig(t)
	done := async(func() reply { return r.send("agent-1", "m-1", "weather?", nil) })
	conv, msgs := r.waitMessages("", 1)
	r.hostOpen(conv.Ref, msgs[0], model.TaskWorking)
	task0 := r.taskIDs()[0]
	r.hostUpdate(task0, model.TaskUpdate{State: model.TaskCompleted, Artifacts: []model.ArtifactUpdate{{
		ID: "a-1", Name: "Weather Report", Content: text("Sunny, 75F"),
	}}})
	res := decode[sendResult](t, await(t, done).ok(t))
	if res.Task == nil || res.Task.ID != task0 || res.Task.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("result = %+v, want the completed task", res)
	}
	if len(res.Task.Artifacts) != 1 || res.Task.Artifacts[0].Name != "Weather Report" || textOf(res.Task.Artifacts[0].Parts) != "Sunny, 75F" {
		t.Fatalf("artifacts = %+v", res.Task.Artifacts)
	}
}

func (r *rig) taskIDs() []string {
	r.t.Helper()
	var out []string
	_ = r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Tasks(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		for _, tk := range p.Items {
			out = append(out, tk.External.ID)
		}
		return err
	})
	return out
}

// A task that ends badly during the wait is still a Task response with that
// state, never the deadline error.
func TestSendMessageBlockingTaskFailedOrCanceledIsTask(t *testing.T) {
	for state, wire := range map[string]string{
		model.TaskFailed: "TASK_STATE_FAILED", model.TaskCanceled: "TASK_STATE_CANCELED", model.TaskRejected: "TASK_STATE_REJECTED",
		model.TaskInputRequired: "TASK_STATE_INPUT_REQUIRED", model.TaskAuthRequired: "TASK_STATE_AUTH_REQUIRED",
	} {
		t.Run(state, func(t *testing.T) {
			r := newRig(t)
			done := async(func() reply { return r.send("agent-1", "m-1", "go", nil) })
			conv, msgs := r.waitMessages("", 1)
			r.hostOpen(conv.Ref, msgs[0], model.TaskWorking)
			r.hostUpdate(r.taskIDs()[0], model.TaskUpdate{State: state})
			res := decode[sendResult](t, await(t, done).ok(t))
			if res.Task == nil || res.Task.Status.State != wire {
				t.Fatalf("result = %+v, want a task in %s", res, wire)
			}
		})
	}
}

// A task that is still active is not a response: the wait goes on, and its
// expiry is the error, never the working task.
func TestSendMessageBlockingExpiryIsDeadlineError(t *testing.T) {
	r := newRig(t, rigOptions{config: []func(*a2a.Config){func(c *a2a.Config) { c.BlockingWait = "300ms" }}})
	start := time.Now()
	e := r.send("agent-1", "m-1", "hello", nil).fail(t, -32603)
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("answered after %v, before the wait ran out", time.Since(start))
	}
	info := e.info(t)
	if info.Reason != "DEADLINE_EXCEEDED" || info.Domain != "uma.hop.top" {
		t.Fatalf("detail = %+v, want DEADLINE_EXCEEDED in uma.hop.top", info)
	}
	conv, msgs := r.waitMessages("", 1)
	if info.Metadata["contextId"] != conv.External[0].ID || info.Metadata["messageId"] != "m-1" {
		t.Fatalf("metadata = %v, want the context and the client's message ID", info.Metadata)
	}
	if _, ok := info.Metadata["taskId"]; ok {
		t.Fatalf("metadata names task %q, but no task exists", info.Metadata["taskId"])
	}
	if r.taskCount() != 0 {
		t.Fatal("expiry created a task")
	}
	// The accepted message stays: a later host open is visible through
	// GetTask, and the retry with the same messageId returns it.
	r.hostOpen(conv.Ref, msgs[0], model.TaskWorking)
	id := r.taskIDs()[0]
	got := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": id}).ok(t))
	if got.Status.State != "TASK_STATE_WORKING" || got.ContextID != conv.External[0].ID {
		t.Fatalf("GetTask = %+v", got)
	}
	// An active task does not settle a retry either; the error then names it.
	e = r.send("agent-1", "m-1", "hello", nil).fail(t, -32603)
	if got := e.info(t).Metadata["taskId"]; got != id {
		t.Fatalf("retry metadata task = %q, want %q", got, id)
	}
	r.hostUpdate(id, model.TaskUpdate{State: model.TaskCompleted})
	res := decode[sendResult](t, r.send("agent-1", "m-1", "hello", nil).ok(t))
	if res.Task == nil || res.Task.ID != id || res.Task.ContextID != conv.External[0].ID || res.Task.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("retry = %+v, want the first context's completed task", res)
	}
	if got := len(r.messages(conv.Ref)); got != 1 {
		t.Fatalf("%d messages after retries, want 1", got)
	}
}

// ErrorDomain moves uma-defined errors only.
func TestErrorDomainOverrideTouchesUmaErrorsOnly(t *testing.T) {
	r := newRig(t, rigOptions{
		options: a2a.Options{Authenticate: headerAuth, ErrorDomain: "errors.acme.example"},
		config:  []func(*a2a.Config){func(c *a2a.Config) { c.BlockingWait = "200ms" }},
	})
	if got := r.send("agent-1", "m-1", "x", nil).fail(t, -32603).info(t); got.Domain != "errors.acme.example" || got.Reason != "DEADLINE_EXCEEDED" {
		t.Fatalf("deadline detail = %+v, want the override", got)
	}
	if got := r.rpc("agent-1", "GetTask", map[string]any{"id": "x"}, "A2A-Version", "0.3").fail(t, -32009).info(t); got.Domain != "a2a-protocol.org" {
		t.Fatalf("version detail = %+v, want the A2A domain", got)
	}
	if got := r.rpc("agent-1", "GetTask", map[string]any{"id": "nope"}).fail(t, -32001).info(t); got.Domain != "a2a-protocol.org" {
		t.Fatalf("not-found detail = %+v, want the A2A domain", got)
	}
	e := r.send("agent-1", "m-2", "x", map[string]any{"contextId": "ghost"}).fail(t, -32602)
	if len(e.Data) != 1 || !strings.Contains(string(e.Data[0]), "google.rpc.BadRequest") || strings.Contains(string(e.Data[0]), "errors.acme") {
		t.Fatalf("invalid-params detail = %s, want an untouched BadRequest", e.Data)
	}
}

// Two requests carrying the same messageId, each with its own freshly
// generated context, end in one context.
func TestRetrySameMessageIdRecoversContext(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "hello", immediately()).ok(t))
	b := decode[sendResult](t, r.send("agent-1", "m-1", "hello", immediately()).ok(t))
	if a.Task.ContextID != b.Task.ContextID || a.Task.ID != b.Task.ID {
		t.Fatalf("retry = %+v, want the first %+v", b.Task, a.Task)
	}
	if r.taskCount() != 1 {
		t.Fatalf("%d tasks, want 1", r.taskCount())
	}
	_ = r.conversation(a.Task.ContextID) // exactly one conversation holds it
	// A different agent's equal messageId is its own message.
	c := decode[sendResult](t, r.send("agent-2", "m-1", "hello", immediately()).ok(t))
	if c.Task.ContextID == a.Task.ContextID {
		t.Fatal("two agents' equal messageIds share a context")
	}
}

func TestClientContextUnknown(t *testing.T) {
	r := newRig(t)
	e := r.send("agent-1", "m-1", "hi", map[string]any{"contextId": "never-seen"}).fail(t, -32602)
	if !strings.Contains(string(e.Data[0]), "message.contextId") {
		t.Fatalf("detail = %s, want the field at fault", e.Data[0])
	}
	if got := len(r.taskIDs()); got != 0 {
		t.Fatalf("%d tasks", got)
	}
	if convs, err := r.engineConversations(); err != nil || len(convs) != 0 {
		t.Fatalf("a refused context created conversations: %v, %v", convs, err)
	}
}

func (r *rig) engineConversations() ([]model.Conversation, error) {
	var out []model.Conversation
	err := r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	return out, err
}

// A context and its tasks belong to the agent that created them. Another
// authenticated agent learns nothing: an unknown context and a foreign one
// are the same -32602, an unknown task and a foreign one the same -32001,
// and nothing is written.
func TestContextAndTaskBelongToTheirOwner(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "mine", immediately()).ok(t)).Task
	before := len(r.messages(r.conversation(a.ContextID).Ref))

	foreign := r.send("agent-2", "m-2", "mine now", map[string]any{"contextId": a.ContextID}).fail(t, -32602)
	unknown := r.send("agent-2", "m-3", "x", map[string]any{"contextId": "ctx-unknown"}).fail(t, -32602)
	if string(foreign.Data[0]) != string(unknown.Data[0]) || foreign.Message != unknown.Message {
		t.Fatalf("foreign %s differs from unknown %s", foreign.Data[0], unknown.Data[0])
	}
	for name, rp := range map[string]reply{
		"GetTask":      r.rpc("agent-2", "GetTask", map[string]any{"id": a.ID}),
		"CancelTask":   r.rpc("agent-2", "CancelTask", map[string]any{"id": a.ID}),
		"continuation": r.send("agent-2", "m-4", "x", map[string]any{"taskId": a.ID}),
	} {
		e := rp.fail(t, -32001)
		if info := e.info(t); info.Reason != "TASK_NOT_FOUND" || info.Metadata["taskId"] != a.ID {
			t.Errorf("%s: detail %+v", name, info)
		}
	}
	e := r.rpc("agent-2", "GetTask", map[string]any{"id": "no-such-task"}).fail(t, -32001)
	if e.Message != "Task not found" {
		t.Fatalf("message = %q", e.Message)
	}
	if l := decode[listResult](t, r.rpc("agent-2", "ListTasks", map[string]any{"contextId": a.ContextID}).ok(t)); len(l.Tasks) != 0 {
		t.Fatalf("agent-2 lists %d of agent-1's tasks", len(l.Tasks))
	}
	if got := len(r.messages(r.conversation(a.ContextID).Ref)); got != before {
		t.Fatalf("conversation holds %d messages, want %d: a refused request wrote", got, before)
	}
	// The owner reaches its own.
	r.send("agent-1", "m-5", "more", map[string]any{
		"contextId": a.ContextID, "taskId": a.ID, "configuration": map[string]any{"returnImmediately": true},
	}).ok(t)
}

func TestContinuation(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "first", immediately()).ok(t)).Task
	b := decode[sendResult](t, r.send("agent-1", "m-2", "second", map[string]any{
		"taskId": a.ID, "configuration": map[string]any{"returnImmediately": true},
	}).ok(t)).Task
	if b == nil || b.ID != a.ID || b.ContextID != a.ContextID {
		t.Fatalf("continuation = %+v, want task %s", b, a.ID)
	}
	if r.taskCount() != 1 {
		t.Fatalf("%d tasks, want the continuation to open none", r.taskCount())
	}
	msgs := r.messages(r.conversation(a.ContextID).Ref)
	if len(msgs) != 2 || msgs[1].Task == nil || msgs[0].Task == nil || *msgs[0].Task != *msgs[1].Task {
		t.Fatalf("messages = %+v, want both bound to the task", msgs)
	}
	// The task's history is the messages bound to it.
	got := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": a.ID, "historyLength": 10}).ok(t))
	if len(got.History) != 2 || textOf(got.History[1].Parts) != "second" || got.History[0].Role != "ROLE_USER" || got.History[1].TaskID != a.ID {
		t.Fatalf("history = %+v", got.History)
	}
	one := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": a.ID, "historyLength": 1}).ok(t))
	if len(one.History) != 1 || textOf(one.History[0].Parts) != "second" {
		t.Fatalf("historyLength 1 = %+v, want the newest message", one.History)
	}
	if none := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": a.ID}).ok(t)); len(none.History) != 0 {
		t.Fatalf("history without historyLength = %+v, want none", none.History)
	}
}

func TestContinuationMismatch(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "first", immediately()).ok(t)).Task
	b := decode[sendResult](t, r.send("agent-1", "m-2", "other", immediately()).ok(t)).Task
	r.send("agent-1", "m-3", "x", map[string]any{"taskId": a.ID, "contextId": b.ContextID}).fail(t, -32602)
}

func TestContinuationTerminal(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "first", immediately()).ok(t)).Task
	r.hostUpdate(a.ID, model.TaskUpdate{State: model.TaskCompleted})
	r.send("agent-1", "m-2", "again", map[string]any{"taskId": a.ID}).fail(t, -32004)
	// The terminal task does not close the context: a new task opens in it.
	b := decode[sendResult](t, r.send("agent-1", "m-3", "new", map[string]any{
		"contextId": a.ContextID, "configuration": map[string]any{"returnImmediately": true},
	}).ok(t)).Task
	if b.ID == a.ID || b.ContextID != a.ContextID {
		t.Fatalf("second task = %+v, want another task in %s", b, a.ContextID)
	}
}

func TestConcurrentSameMessageIdOneTask(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	results := make([]reply, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.send("agent-1", "m-1", "hello", immediately())
		}()
	}
	wg.Wait()
	var first *task
	for i, rp := range results {
		res := decode[sendResult](t, rp.ok(t))
		if first == nil {
			first = res.Task
		}
		if res.Task.ID != first.ID || res.Task.ContextID != first.ContextID {
			t.Fatalf("request %d = %+v, want %+v", i, res.Task, first)
		}
	}
	if r.taskCount() != 1 {
		t.Fatalf("%d tasks, want 1", r.taskCount())
	}
	if n := len(r.messages(r.conversation(first.ContextID).Ref)); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}

func TestSendMessageValidation(t *testing.T) {
	r := newRig(t)
	good := func() map[string]any {
		return map[string]any{"messageId": "m", "role": "ROLE_USER", "parts": []map[string]any{{"text": "x"}}}
	}
	with := func(k string, v any) map[string]any { m := good(); m[k] = v; return m }
	for name, msg := range map[string]map[string]any{
		"no message id":    with("messageId", ""),
		"agent role":       with("role", "ROLE_AGENT"),
		"no role":          with("role", ""),
		"no parts":         with("parts", []any{}),
		"empty text":       with("parts", []map[string]any{{"text": ""}}),
		"two fields":       with("parts", []map[string]any{{"text": "x", "url": "https://e.example/f"}}),
		"no field":         with("parts", []map[string]any{{"filename": "f"}}),
		"bad raw":          with("parts", []map[string]any{{"raw": "***"}}),
		"blank task ref":   with("referenceTaskIds", []string{""}),
		"control in msgid": with("messageId", "a\nb"),
	} {
		t.Run(name, func(t *testing.T) {
			r.rpc("agent-1", "SendMessage", map[string]any{"message": msg}).fail(t, -32602)
		})
	}
	if len(r.taskIDs()) != 0 {
		t.Fatal("a refused message opened a task")
	}
	r.rpc("agent-1", "SendMessage", map[string]any{}).fail(t, -32602)
}

// Data parts become structured parts, url parts structured file references
// that are never fetched, raw parts staged attachments; referenced tasks
// are kept as references and nothing else.
func TestSendMessageParts(t *testing.T) {
	r := newRig(t)
	raw := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	res := decode[sendResult](t, r.rpc("agent-1", "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m-1", "role": "ROLE_USER",
			"parts": []map[string]any{
				{"text": "look"},
				{"data": map[string]any{"k": []int{1, 2}}},
				{"url": "http://127.0.0.1:1/never-fetched", "mediaType": "image/png", "filename": "u.png"},
				{"raw": raw, "mediaType": "image/png", "filename": "in.png"},
			},
			"referenceTaskIds": []string{"earlier-task"},
		},
		"configuration": map[string]any{"returnImmediately": true},
	}).ok(t))
	conv := r.conversation(res.Task.ContextID)
	msgs := r.messages(conv.Ref)
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	parts := msgs[0].Content.Parts
	if len(parts) != 4 || parts[0].Kind != "text" || parts[1].Structured == nil || parts[2].Structured == nil || parts[3].Attachment == nil {
		t.Fatalf("parts = %+v", parts)
	}
	if s := parts[1].Structured; s.Name != a2a.SchemaData || s.Version != 1 || string(s.Data) != `{"k":[1,2]}` {
		t.Fatalf("data part = %+v", s)
	}
	if s := parts[2].Structured; s.Name != a2a.SchemaFileURL || s.Version != 1 {
		t.Fatalf("url part = %+v", s)
	} else {
		var f map[string]string
		_ = json.Unmarshal(s.Data, &f)
		if f["url"] != "http://127.0.0.1:1/never-fetched" || f["media_type"] != "image/png" || f["filename"] != "u.png" {
			t.Fatalf("url data = %v", f)
		}
	}
	// The history shows the parts back, attachments by reference.
	h := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": res.Task.ID, "historyLength": 5}).ok(t))
	hp := h.History[0].Parts
	if len(hp) != 4 || textOf(hp) != "look" || string(hp[1].Data) != `{"k":[1,2]}` || hp[2].URL == nil || *hp[2].URL != "http://127.0.0.1:1/never-fetched" || hp[3].Data == nil {
		t.Fatalf("history parts = %+v", hp)
	}
	// A retry stages the same bytes under the same key: still one attachment.
	r.rpc("agent-1", "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m-1", "role": "ROLE_USER",
			"parts": []map[string]any{{"text": "look"}, {"data": map[string]any{"k": []int{1, 2}}},
				{"url": "http://127.0.0.1:1/never-fetched", "mediaType": "image/png", "filename": "u.png"},
				{"raw": raw, "mediaType": "image/png", "filename": "in.png"}},
			"referenceTaskIds": []string{"earlier-task"},
		},
		"configuration": map[string]any{"returnImmediately": true},
	}).ok(t)
	if n := len(r.messages(conv.Ref)); n != 1 {
		t.Fatalf("%d messages after the retry", n)
	}
}

func TestStatusContentAndArtifactsRender(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "book a flight", immediately()).ok(t)).Task
	status := text("Where would you like to fly from and to?")
	r.hostUpdate(a.ID, model.TaskUpdate{State: model.TaskInputRequired, StatusMessage: &status})
	got := decode[task](t, r.rpc("agent-1", "GetTask", map[string]any{"id": a.ID}).ok(t))
	if got.Status.State != "TASK_STATE_INPUT_REQUIRED" || got.Status.Message == nil {
		t.Fatalf("task = %+v", got)
	}
	m := got.Status.Message
	if m.Role != "ROLE_AGENT" || textOf(m.Parts) != "Where would you like to fly from and to?" || m.TaskID != a.ID || m.ContextID != a.ContextID {
		t.Fatalf("status message = %+v", m)
	}
}
