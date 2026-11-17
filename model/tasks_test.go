package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	ctxRef   = ExternalRef{Channel: chanA, Namespace: "a2a.context", Kind: RefKindContext, ID: "ctx-1"}
	taskRefA = TaskRef{Conversation: convA, ID: idD}
)

func validTask() Task {
	return Task{
		Ref:           taskRefA,
		Context:       ctxRef,
		External:      TaskExternalRef(chanA, taskRefA.ID),
		State:         TaskWorking,
		StatusMessage: &msgA,
		StatusAt:      goldenAt.Add(time.Second),
		CreatedAt:     goldenAt,
		Artifacts: []Artifact{
			{ID: "a1", Name: "report", Description: "the report", Content: text, Complete: true},
			{ID: "a2", Content: contentWithAttachment(inboxA)},
		},
		Revision: "r1",
	}
}

func TestTaskStates(t *testing.T) {
	stored := []string{
		TaskSubmitted, TaskWorking, TaskInputRequired, TaskAuthRequired,
		TaskCompleted, TaskFailed, TaskCanceled, TaskRejected,
	}
	want := "submitted,working,input_required,auth_required,completed,failed,canceled,rejected"
	if got := strings.Join(stored, ","); got != want {
		t.Fatalf("states = %s, want %s", got, want)
	}
	if TaskUnspecified != "unspecified" {
		t.Fatalf("TaskUnspecified = %q", TaskUnspecified)
	}
	for _, s := range stored {
		if !IsTaskState(s) {
			t.Errorf("%s not a task state", s)
		}
	}
	for _, s := range []string{TaskUnspecified, "", "done", "WORKING"} {
		if IsTaskState(s) {
			t.Errorf("%q accepted as a task state", s)
		}
	}
	terminal := map[string]bool{TaskCompleted: true, TaskFailed: true, TaskCanceled: true, TaskRejected: true}
	interrupted := map[string]bool{TaskInputRequired: true, TaskAuthRequired: true}
	for _, s := range append(stored, TaskUnspecified, "") {
		if IsTerminalTaskState(s) != terminal[s] {
			t.Errorf("IsTerminalTaskState(%q) = %v", s, !terminal[s])
		}
		if IsInterruptedTaskState(s) != interrupted[s] {
			t.Errorf("IsInterruptedTaskState(%q) = %v", s, !interrupted[s])
		}
	}
}

func TestTaskTransitions(t *testing.T) {
	stored := []string{
		TaskSubmitted, TaskWorking, TaskInputRequired, TaskAuthRequired,
		TaskCompleted, TaskFailed, TaskCanceled, TaskRejected,
	}
	for _, from := range stored {
		for _, to := range append(stored, TaskUnspecified, "", "done") {
			var want bool
			switch {
			case !IsTaskState(to), IsTerminalTaskState(from):
				want = false
			case to == TaskSubmitted:
				want = from == TaskSubmitted
			default:
				want = true
			}
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"", TaskSubmitted, true},
		{"", TaskWorking, true},
		{"", TaskInputRequired, true},
		{"", TaskCompleted, true},
		{"", TaskUnspecified, false},
		{TaskUnspecified, TaskWorking, false},
		{"done", TaskWorking, false},
		{TaskInputRequired, TaskWorking, true},
		{TaskAuthRequired, TaskInputRequired, true},
		{TaskSubmitted, TaskCompleted, true},
		{TaskWorking, TaskSubmitted, false},
		{TaskCompleted, TaskCompleted, false},
		{TaskCanceled, TaskWorking, false},
	} {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTaskExternalRef(t *testing.T) {
	r := TaskExternalRef(chanA, idD)
	requireOK(t, r.Validate())
	if r.Channel != chanA || r.Namespace != ProviderMessageNamespace || r.Kind != RefKindTask || r.ID != string(idD) || r.ParentID != "" {
		t.Fatalf("TaskExternalRef = %+v", r)
	}
	if RefKindTask != "task" {
		t.Fatalf("RefKindTask = %q", RefKindTask)
	}
}

func TestTaskValidate(t *testing.T) {
	requireOK(t, validTask().Validate())
	minimal := validTask()
	minimal.StatusMessage, minimal.Artifacts, minimal.StatusAt = nil, nil, minimal.CreatedAt
	requireOK(t, minimal.Validate())
	requested := validTask()
	requested.CancelRequested = true
	requireOK(t, requested.Validate())

	otherConv := MessageRef{Conversation: convB, ID: idC}
	cases := map[string]struct {
		mut   func(*Task)
		field string
	}{
		"bad ref":                    {func(x *Task) { x.Ref.ID = "" }, "ref.id"},
		"undeclared state":           {func(x *Task) { x.State = "done" }, "state"},
		"unspecified state":          {func(x *Task) { x.State = TaskUnspecified }, "state"},
		"no state":                   {func(x *Task) { x.State = "" }, "state"},
		"context not a context":      {func(x *Task) { x.Context.Kind = RefKindRoom }, "context.kind"},
		"bad context":                {func(x *Task) { x.Context.Namespace = "" }, "context.namespace"},
		"external another id":        {func(x *Task) { x.External.ID = "other" }, "external"},
		"external another channel":   {func(x *Task) { x.External.Channel = channel }, "external"},
		"external provider owned":    {func(x *Task) { x.External.Namespace = "a2a.task" }, "external"},
		"status message elsewhere":   {func(x *Task) { x.StatusMessage = &otherConv }, "status_message"},
		"bad status message":         {func(x *Task) { x.StatusMessage = &MessageRef{Conversation: convA} }, "status_message.id"},
		"no created at":              {func(x *Task) { x.CreatedAt = time.Time{} }, "created_at"},
		"no status at":               {func(x *Task) { x.StatusAt = time.Time{} }, "status_at"},
		"status before creation":     {func(x *Task) { x.StatusAt = x.CreatedAt.Add(-time.Second) }, "status_at"},
		"canceled still requested":   {func(x *Task) { x.State, x.CancelRequested = TaskCanceled, true }, "cancel_requested"},
		"artifact without ID":        {func(x *Task) { x.Artifacts[1].ID = "" }, "artifacts[1].id"},
		"artifact ID with NUL":       {func(x *Task) { x.Artifacts[0].ID = "a\x00" }, "artifacts[0].id"},
		"duplicate artifact":         {func(x *Task) { x.Artifacts[1].ID = "a1" }, "artifacts[1].id"},
		"artifact without content":   {func(x *Task) { x.Artifacts[0].Content = Content{} }, "artifacts[0].content.parts"},
		"artifact foreign file":      {func(x *Task) { x.Artifacts[1].Content = contentWithAttachment(inboxB) }, "artifacts[1].content.parts[0].attachment"},
		"artifact name too long":     {func(x *Task) { x.Artifacts[0].Name = strings.Repeat("n", maxTaskTextBytes+1) }, "artifacts[0].name"},
		"artifact description NUL":   {func(x *Task) { x.Artifacts[0].Description = "\x00" }, "artifacts[0].description"},
		"too many artifacts":         {func(x *Task) { x.Artifacts = manyArtifacts(maxTaskArtifacts + 1) }, "artifacts"},
		"bad context channel tenant": {func(x *Task) { x.Context.Channel.Tenant = "" }, "context.channel.tenant"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			x := validTask()
			c.mut(&x)
			requireError(t, x.Validate(), Validation, c.field)
		})
	}
}

func manyArtifacts(n int) []Artifact {
	out := make([]Artifact, n)
	for i := range out {
		out[i] = Artifact{ID: "a" + strconv.Itoa(i), Content: text}
	}
	return out
}

func TestTaskCloneDeep(t *testing.T) {
	x := validTask()
	c := x.Clone()
	c.StatusMessage.ID = "other"
	c.Artifacts[0].Content.Parts[0].Text = "changed"
	c.Artifacts[1].Content.Parts[0].Attachment.ID = "other"
	if x.StatusMessage.ID != idC || x.Artifacts[0].Content.Parts[0].Text != "hello" || x.Artifacts[1].Content.Parts[0].Attachment.ID != idD {
		t.Fatal("Task.Clone aliases the original")
	}
}

func TestTaskBindingValidate(t *testing.T) {
	ext := TaskExternalRef(chanA, idD)
	requireOK(t, TaskBinding{Open: true}.Validate())
	requireOK(t, TaskBinding{External: ext}.Validate())
	for name, c := range map[string]struct {
		b     TaskBinding
		field string
	}{
		"empty":           {TaskBinding{}, "task"},
		"open with ref":   {TaskBinding{Open: true, External: ext}, "task.external"},
		"ref not a task":  {TaskBinding{External: ctxRef}, "task.external.kind"},
		"malformed ref":   {TaskBinding{External: ExternalRef{Channel: chanA, Kind: RefKindTask, ID: "t"}}, "task.external.namespace"},
		"ref without ID":  {TaskBinding{External: ExternalRef{Channel: chanA, Namespace: "uma.provider", Kind: RefKindTask}}, "task.external.id"},
		"open zero chan":  {TaskBinding{Open: true, External: ExternalRef{ID: "x"}}, "task.external"},
		"ref zero tenant": {TaskBinding{External: ExternalRef{Namespace: "uma.provider", Kind: RefKindTask, ID: "t"}}, "task.external.channel.tenant"},
	} {
		t.Run(name, func(t *testing.T) { requireError(t, c.b.Validate(), Validation, c.field) })
	}
}

func TestEnvelopeTaskBindingCloned(t *testing.T) {
	env := Envelope{Task: &TaskBinding{External: TaskExternalRef(chanA, idD)}}
	c := env.Clone()
	c.Task.External.ID = "other"
	if env.Task.External.ID != string(idD) || c.Task == env.Task {
		t.Fatal("Envelope.Clone aliases the task binding")
	}
}

func TestIngressReceiptValidate(t *testing.T) {
	conv, msg, tk, cx := convA, msgA, taskRefA, ctxRef
	full := IngressReceipt{ID: idA, Durable: true, Committed: true, Conversation: &conv, Context: &cx, Message: &msg, Task: &tk}
	requireOK(t, full.Validate())
	dup := full
	dup.Duplicate = true
	requireOK(t, dup.Validate())
	requireOK(t, IngressReceipt{ID: idA, Duplicate: true}.Validate())
	requireOK(t, IngressReceipt{ID: idA, Committed: true}.Validate())
	requireOK(t, IngressReceipt{ID: idA, Committed: true, Conversation: &conv, Message: &msg}.Validate())
	// A status report's receipt names an operation and no conversation.
	requireOK(t, IngressReceipt{ID: idA, Committed: true, Operation: &opRef}.Validate())
	requireOK(t, IngressReceipt{ID: idA, Committed: true, Duplicate: true, Operation: &opRef}.Validate())

	otherMsg := MessageRef{Conversation: convB, ID: idC}
	otherTask := TaskRef{Conversation: convB, ID: idD}
	room := ExternalRef{Channel: chanA, Namespace: "a2a.context", Kind: RefKindRoom, ID: "r"}
	foreign := ExternalRef{Channel: ChannelRef{Tenant: tenantB, ID: idA}, Namespace: "a2a.context", Kind: RefKindContext, ID: "c"}
	for name, c := range map[string]struct {
		r     IngressReceipt
		field string
	}{
		"rejected with conversation": {IngressReceipt{ID: idA, Conversation: &conv}, "conversation"},
		"unprocessed duplicate refs": {IngressReceipt{ID: idA, Duplicate: true, Conversation: &conv, Message: &msg}, "conversation"},
		"rejected with message only": {IngressReceipt{ID: idA, Message: &msg}, "message"},
		"rejected with task only":    {IngressReceipt{ID: idA, Task: &tk}, "task"},
		"rejected with context only": {IngressReceipt{ID: idA, Context: &cx}, "context"},
		"message without conv":       {IngressReceipt{ID: idA, Committed: true, Message: &msg}, "message"},
		"task without conv":          {IngressReceipt{ID: idA, Committed: true, Task: &tk}, "task"},
		"context without conv":       {IngressReceipt{ID: idA, Committed: true, Context: &cx}, "context"},
		"message elsewhere":          {IngressReceipt{ID: idA, Committed: true, Conversation: &conv, Message: &otherMsg}, "message"},
		"task elsewhere":             {IngressReceipt{ID: idA, Committed: true, Conversation: &conv, Task: &otherTask}, "task"},
		"context not a context":      {IngressReceipt{ID: idA, Committed: true, Conversation: &conv, Context: &room}, "context.kind"},
		"context another tenant":     {IngressReceipt{ID: idA, Committed: true, Conversation: &conv, Context: &foreign}, "context.channel"},
		"bad conversation":           {IngressReceipt{ID: idA, Committed: true, Conversation: &ConversationRef{Inbox: inboxA}}, "conversation.id"},
		"bad id":                     {IngressReceipt{ID: badID, Committed: true}, "id"},
		"rejected with operation":    {IngressReceipt{ID: idA, Operation: &opRef}, "operation"},
		"bad operation":              {IngressReceipt{ID: idA, Committed: true, Operation: &OperationRef{Tenant: tenantA}}, "operation.id"},
	} {
		t.Run(name, func(t *testing.T) { requireError(t, c.r.Validate(), Validation, c.field) })
	}
}

func TestIngressReceiptEqualAndClone(t *testing.T) {
	conv, msg, tk, cx := convA, msgA, taskRefA, ctxRef
	op := opRef
	a := IngressReceipt{ID: idA, Committed: true, Conversation: &conv, Context: &cx, Message: &msg, Task: &tk, Operation: &op}
	b := a.Clone()
	if !a.Equal(b) || !b.Equal(a) {
		t.Fatal("clone not equal")
	}
	if a.Conversation == b.Conversation || a.Context == b.Context || a.Message == b.Message || a.Task == b.Task || a.Operation == b.Operation {
		t.Fatal("IngressReceipt.Clone aliases references")
	}
	var back IngressReceipt
	raw, err := json.Marshal(a)
	requireOK(t, err)
	requireOK(t, json.Unmarshal(raw, &back))
	if !a.Equal(back) {
		t.Fatalf("decoded receipt %+v not equal to %+v", back, a)
	}
	for name, mut := range map[string]func(*IngressReceipt){
		"id":           func(r *IngressReceipt) { r.ID = idB },
		"durable":      func(r *IngressReceipt) { r.Durable = true },
		"committed":    func(r *IngressReceipt) { r.Committed = false },
		"duplicate":    func(r *IngressReceipt) { r.Duplicate = true },
		"conversation": func(r *IngressReceipt) { c := convB; r.Conversation = &c },
		"no context":   func(r *IngressReceipt) { r.Context = nil },
		"message":      func(r *IngressReceipt) { m := MessageRef{Conversation: convA, ID: idA}; r.Message = &m },
		"task":         func(r *IngressReceipt) { r.Task = nil },
		"no operation": func(r *IngressReceipt) { r.Operation = nil },
		"operation":    func(r *IngressReceipt) { o := OperationRef{Tenant: tenantA, ID: idC}; r.Operation = &o },
	} {
		t.Run(name, func(t *testing.T) {
			c := a.Clone()
			mut(&c)
			if a.Equal(c) || c.Equal(a) {
				t.Fatalf("receipts differing in %s compare equal", name)
			}
		})
	}
	if !(IngressReceipt{}).Equal(IngressReceipt{}) {
		t.Fatal("zero receipts differ")
	}
}

func TestFilterTask(t *testing.T) {
	tk := taskRefA
	requireOK(t, Filter{Task: &tk}.Validate())
	requireError(t, Filter{Task: &TaskRef{Conversation: convA}}.Validate(), Validation, "task.id")
	f := Filter{Task: &tk}
	c := f.Clone()
	c.Task.ID = "other"
	if f.Task.ID != idD {
		t.Fatal("Filter.Clone aliases the task")
	}
}

func TestOperationResultTaskCloned(t *testing.T) {
	tk := taskRefA
	o := Operation{ResultTask: &tk}
	c := o.Clone()
	c.ResultTask.ID = "other"
	if o.ResultTask.ID != idD {
		t.Fatal("Operation.Clone aliases ResultTask")
	}
}

func TestMessageInReplyToCloned(t *testing.T) {
	parent := msgA
	m := Message{InReplyTo: &parent}
	c := m.Clone()
	c.InReplyTo.ID = "other"
	if m.InReplyTo.ID != idC {
		t.Fatal("Message.Clone aliases InReplyTo")
	}
}

// TestTaskUpdatedPayloadWire: unset optional references are absent, the
// summary fields always present.
func TestTaskUpdatedPayloadWire(t *testing.T) {
	b, err := json.Marshal(TaskUpdatedPayload{Task: taskRefA, Changes: []string{TaskFieldCreated}, Trigger: TriggerProvider})
	requireOK(t, err)
	var fields map[string]json.RawMessage
	requireOK(t, json.Unmarshal(b, &fields))
	for _, k := range []string{"state", "operation", "message"} {
		if _, ok := fields[k]; ok {
			t.Errorf("%s present in %s", k, b)
		}
	}
	for _, k := range []string{"task", "changes", "trigger", "previous_revision"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("%s missing from %s", k, b)
		}
	}
	if got := strings.Join([]string{
		TaskFieldCreated, TaskFieldState, TaskFieldStatusMessage, TaskFieldArtifacts,
		TaskFieldCancelRequested, TaskFieldExternal,
	}, ","); got != "created,state,status_message,artifacts,cancel_requested,external" {
		t.Fatalf("task fields = %s", got)
	}
	if MessageOriginTaskStatus != "task_status" || EventTaskUpdated != "task.updated" || SchemaTaskUpdated != "uma.task.updated" {
		t.Fatalf("names = %s %s %s", MessageOriginTaskStatus, EventTaskUpdated, SchemaTaskUpdated)
	}
}
