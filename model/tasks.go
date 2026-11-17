package model

// Agent tasks.
//
// A task is work the host executes for a remote agent in a per-agent-context
// conversation. uma keeps its protocol state and correlation; the host opens
// tasks and reports their progress (Tasks.Act), never uma itself. A
// conversation holds any number of tasks, and one task's lifecycle never
// changes another's or the conversation's status.

// Task states (Task.State). Submitted and working are active, input and
// auth required interrupted (the task waits for the remote agent), the rest
// terminal: a terminal task never changes state again. TaskUnspecified is
// declared for protocol completeness only: it is never stored and never a
// transition target.
const (
	TaskSubmitted     = "submitted"
	TaskWorking       = "working"
	TaskInputRequired = "input_required"
	TaskAuthRequired  = "auth_required"
	TaskCompleted     = "completed"
	TaskFailed        = "failed"
	TaskCanceled      = "canceled"
	TaskRejected      = "rejected"
	TaskUnspecified   = "unspecified"
)

// Task limits.
const (
	maxTaskTextBytes = 8 << 10
	maxTaskArtifacts = 256
)

// IsTaskState reports whether s is a state a task can hold. TaskUnspecified
// is not one.
func IsTaskState(s string) bool {
	switch s {
	case TaskSubmitted, TaskWorking:
		return true
	}
	return IsInterruptedTaskState(s) || IsTerminalTaskState(s)
}

// IsTerminalTaskState reports whether s is a terminal task state:
// completed, failed, canceled or rejected.
func IsTerminalTaskState(s string) bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskCanceled, TaskRejected:
		return true
	}
	return false
}

// IsInterruptedTaskState reports whether s is an interrupted task state:
// input or auth required.
func IsInterruptedTaskState(s string) bool {
	return s == TaskInputRequired || s == TaskAuthRequired
}

// CanTransition reports whether a task may move from state from to state
// to. An empty from is opening a task, which may start in any state
// (submitted by default). A terminal state is final. Submitted is reachable
// only on opening or as a same-state update of a submitted task; every other
// move between non-terminal states, same-state updates included, is
// allowed. A target that is not a task state (TaskUnspecified included) is
// never allowed.
func CanTransition(from, to string) bool {
	switch {
	case !IsTaskState(to):
		return false
	case from == "":
		return true
	case !IsTaskState(from), IsTerminalTaskState(from):
		return false
	case to == TaskSubmitted:
		return from == TaskSubmitted
	}
	return true
}

// TaskExternalRef returns the core reference of the task with uma ID id on
// channel ch (Task.External). The engine allocates task IDs, so the
// reference lives in the core provider namespace, never an adapter's; a
// protocol adapter shows its ID as the task ID.
func TaskExternalRef(ch ChannelRef, id ID) ExternalRef {
	return ExternalRef{Channel: ch, Namespace: ProviderMessageNamespace, Kind: RefKindTask, ID: string(id)}
}

// Artifact is one output of a task. ID is chosen by the host, unique within
// the task; Name and Description are optional. Content holds its parts:
// text, attachments of the task's inbox, or structured data. Complete
// reports that the last chunk arrived.
type Artifact struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Content     Content `json:"content"`
	Complete    bool    `json:"complete"`
}

func (a Artifact) validate(path string, in InboxRef) error {
	return firstError(
		checkRaw(fieldPath(path, "id"), a.ID, maxTokenBytes),
		optionalText(fieldPath(path, "name"), a.Name, maxTaskTextBytes),
		optionalText(fieldPath(path, "description"), a.Description, maxTaskTextBytes),
		a.Content.validate(fieldPath(path, "content"), DefaultContentLimits(), &in),
	)
}

// checkArtifactIDs checks the count and that IDs are unique.
func checkArtifactIDs(field string, ids []string) error {
	if len(ids) > maxTaskArtifacts {
		return invalidField(field, "too many artifacts")
	}
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if seen[id] {
			return invalidField(fieldPath(indexPath(field, i), "id"), "duplicate artifact")
		}
		seen[id] = true
	}
	return nil
}

// Validate checks a task record: its ref, a stored state, a context
// reference of kind context, the core External reference of its ID on the
// context's channel, a status message in the task's conversation, creation
// and status times (status never before creation), that a canceled task has
// no pending cancel request, and artifacts unique by ID whose attachments
// belong to the task's inbox.
func (t Task) Validate() error {
	var state, ctxKind, external, status, cancel error
	if !IsTaskState(t.State) {
		state = invalidField("state", "must be a declared task state")
	}
	if t.Context.Kind != RefKindContext {
		ctxKind = invalidField("context.kind", "must be context")
	}
	if t.External != TaskExternalRef(t.Context.Channel, t.Ref.ID) {
		external = invalidField("external", "must be the task's core reference")
	}
	if t.StatusMessage != nil {
		status = t.StatusMessage.validate("status_message", false)
		if status == nil && !t.StatusMessage.In(t.Ref.Conversation) {
			status = invalidField("status_message", "must belong to the task's conversation")
		}
	}
	times := requireTime("created_at", t.CreatedAt.IsZero())
	if times == nil {
		times = requireTime("status_at", t.StatusAt.IsZero())
	}
	if times == nil && t.StatusAt.Before(t.CreatedAt) {
		times = invalidField("status_at", "must not precede created_at")
	}
	if t.CancelRequested && t.State == TaskCanceled {
		cancel = invalidField("cancel_requested", "a canceled task has no pending request")
	}
	ids := make([]string, len(t.Artifacts))
	for i, a := range t.Artifacts {
		ids[i] = a.ID
	}
	artifacts := checkArtifactIDs("artifacts", ids)
	for i, a := range t.Artifacts {
		if artifacts != nil {
			break
		}
		artifacts = a.validate(indexPath("artifacts", i), t.Ref.Conversation.Inbox)
	}
	return firstError(
		t.Ref.validate("ref", false),
		state,
		t.Context.validate("context"),
		ctxKind,
		external,
		status,
		times,
		cancel,
		artifacts,
	)
}

// TaskBinding binds an inbound message (Envelope.Task) to a task: External
// names an existing task by its core reference (a continuation), or Open
// asks the engine to open a task for the message. Exactly one is set.
type TaskBinding struct {
	External ExternalRef `json:"external"`
	Open     bool        `json:"open"`
}

// Validate checks that the binding either opens a task, naming none, or
// names a task by a reference of kind task.
func (b TaskBinding) Validate() error {
	switch {
	case b.Open && !b.External.IsZero():
		return invalidField("task.external", "an opening binding names no task")
	case b.Open:
		return nil
	case b.External.IsZero():
		return invalidField("task", "must open a task or name one")
	}
	if err := b.External.validate("task.external"); err != nil {
		return err
	}
	if b.External.Kind != RefKindTask {
		return invalidField("task.external.kind", "must be task")
	}
	return nil
}

// Validate checks the receipt's references: each well formed, carried only
// by a committed receipt, under a Conversation they belong to, and Context
// of kind context in the conversation's tenant.
func (r IngressReceipt) Validate() error {
	refs := []struct {
		field string
		set   bool
	}{
		{"conversation", r.Conversation != nil},
		{"context", r.Context != nil},
		{"message", r.Message != nil},
		{"task", r.Task != nil},
	}
	for _, f := range refs {
		if f.set && !r.Committed {
			return invalidField(f.field, "only a committed receipt carries references")
		}
	}
	if r.ID != "" {
		if err := checkID("id", r.ID); err != nil {
			return err
		}
	}
	if r.Operation != nil {
		if !r.Committed {
			return invalidField("operation", "only a committed receipt carries references")
		}
		if err := r.Operation.validate("operation", false); err != nil {
			return err
		}
	}
	if r.Conversation == nil {
		for _, f := range refs[1:] {
			if f.set {
				return invalidField(f.field, "requires the conversation")
			}
		}
		return nil
	}
	conv := *r.Conversation
	if err := conv.validate("conversation", false); err != nil {
		return err
	}
	if r.Context != nil {
		if err := r.Context.validate("context"); err != nil {
			return err
		}
		if r.Context.Kind != RefKindContext {
			return invalidField("context.kind", "must be context")
		}
		if r.Context.Scope() != conv.Scope() {
			return invalidField("context.channel", "must be in the conversation's tenant")
		}
	}
	if r.Message != nil {
		if err := r.Message.validate("message", false); err != nil {
			return err
		}
		if !r.Message.In(conv) {
			return invalidField("message", "must belong to the conversation")
		}
	}
	if r.Task != nil {
		if err := r.Task.validate("task", false); err != nil {
			return err
		}
		if !r.Task.In(conv) {
			return invalidField("task", "must belong to the conversation")
		}
	}
	return nil
}

// Equal reports whether r and o hold the same values, comparing references
// by value: a stored receipt decoded again equals the one stored.
func (r IngressReceipt) Equal(o IngressReceipt) bool {
	return r.ID == o.ID && r.Durable == o.Durable && r.Committed == o.Committed && r.Duplicate == o.Duplicate &&
		equalPtr(r.Conversation, o.Conversation) && equalPtr(r.Context, o.Context) &&
		equalPtr(r.Message, o.Message) && equalPtr(r.Task, o.Task) && equalPtr(r.Operation, o.Operation)
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
