package tasks_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/tasks"
	"hop.top/uma/model"
)

var (
	t0   = time.Unix(1_700_000_000, 0).UTC()
	t1   = t0.Add(time.Minute)
	ch   = model.ChannelRef{Tenant: "tenant", ID: "00000000-0000-7000-8000-000000000001"}
	conv = model.ConversationRef{Inbox: model.InboxRef{Tenant: "tenant", ID: "00000000-0000-7000-8000-000000000002"}, ID: "00000000-0000-7000-8000-000000000003"}
	ctx  = model.ExternalRef{Channel: ch, Namespace: "test.context", Kind: model.RefKindContext, ID: "ctx-1"}
)

func requireCode(t *testing.T, err error, code model.ErrorCode, want string) {
	t.Helper()
	var e *model.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	if !strings.Contains(e.SafeMessage, want) {
		t.Fatalf("message %q, want it to contain %q", e.SafeMessage, want)
	}
}

func agentConv() model.Conversation {
	return model.Conversation{Ref: conv, Mode: model.ModePerAgentContext, ReplyRoute: ctx, External: []model.ExternalRef{ctx}}
}

func msg(id model.ID) model.Message {
	return model.Message{Ref: model.MessageRef{Conversation: conv, ID: id}}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func task(state string) model.Task {
	return model.Task{Ref: model.TaskRef{Conversation: conv, ID: "task-1"}, State: state, StatusAt: t0, CreatedAt: t0, Revision: "r1"}
}

// Tasks open only in per-agent-context conversations that carry a context
// reference, for a message of the conversation without a task, in any
// state CanTransition allows from none.
func TestOpen(t *testing.T) {
	c, err := tasks.Open(agentConv(), msg("m1"), model.TaskOpen{}, "task-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	want := model.Task{
		Ref: model.TaskRef{Conversation: conv, ID: "task-1"}, Context: ctx,
		External: model.TaskExternalRef(ch, "task-1"), State: model.TaskSubmitted, StatusAt: t0, CreatedAt: t0,
	}
	if c.Task.Ref != want.Ref || c.Task.Context != want.Context || c.Task.External != want.External ||
		c.Task.State != want.State || !c.Task.StatusAt.Equal(t0) || !c.Task.CreatedAt.Equal(t0) {
		t.Fatalf("task = %+v", c.Task)
	}
	if !slices.Equal(c.Changes, []string{model.TaskFieldCreated}) || *c.State != (model.StatusChange{To: model.TaskSubmitted}) {
		t.Fatalf("change = %+v", c)
	}
	if c, err := tasks.Open(agentConv(), msg("m1"), model.TaskOpen{State: model.TaskWorking}, "task-1", t0); err != nil || c.Task.State != model.TaskWorking {
		t.Fatalf("open working = %+v, %v", c, err)
	}

	other := agentConv()
	other.Mode = model.ModePerRoom
	_, err = tasks.Open(other, msg("m1"), model.TaskOpen{}, "task-1", t0)
	requireCode(t, err, model.Validation, "per-agent-context")
	bare := agentConv()
	bare.ReplyRoute, bare.External = model.ExternalRef{}, nil
	_, err = tasks.Open(bare, msg("m1"), model.TaskOpen{}, "task-1", t0)
	requireCode(t, err, model.Validation, "no agent context")
	bound := msg("m1")
	bound.Task = &model.TaskRef{Conversation: conv, ID: "task-0"}
	_, err = tasks.Open(agentConv(), bound, model.TaskOpen{}, "task-1", t0)
	requireCode(t, err, model.Conflict, "already bound")
	_, err = tasks.Open(agentConv(), msg("m1"), model.TaskOpen{State: model.TaskUnspecified}, "task-1", t0)
	requireCode(t, err, model.Validation, "state")
}

// The context reference comes from the reply route, else the external refs.
func TestContextFromExternal(t *testing.T) {
	c := agentConv()
	c.ReplyRoute = model.ExternalRef{}
	got, err := tasks.Context(c)
	if err != nil || got != ctx {
		t.Fatalf("Context = %v, %v", got, err)
	}
}

// Terminal is final; submitted is never re-entered; interrupted resumes.
func TestUpdateTransitions(t *testing.T) {
	for _, s := range []string{model.TaskCompleted, model.TaskFailed, model.TaskCanceled, model.TaskRejected} {
		_, err := tasks.Update(task(s), model.TaskUpdate{State: model.TaskWorking}, t1)
		requireCode(t, err, model.Conflict, "terminal")
		_, err = tasks.Update(task(s), model.TaskUpdate{StatusMessage: ptr(text("late"))}, t1)
		requireCode(t, err, model.Conflict, "terminal")
		_, err = tasks.Update(task(s), model.TaskUpdate{State: s}, t1)
		requireCode(t, err, model.Conflict, "terminal")
	}
	_, err := tasks.Update(task(model.TaskWorking), model.TaskUpdate{State: model.TaskSubmitted}, t1)
	requireCode(t, err, model.Conflict, "cannot move from working to submitted")

	c, err := tasks.Update(task(model.TaskInputRequired), model.TaskUpdate{State: model.TaskWorking}, t1)
	if err != nil || c.Task.State != model.TaskWorking || !c.Task.StatusAt.Equal(t1) || c.Previous != "r1" ||
		!slices.Equal(c.Changes, []string{model.TaskFieldState}) ||
		*c.State != (model.StatusChange{From: model.TaskInputRequired, To: model.TaskWorking}) {
		t.Fatalf("resume = %+v, %v", c, err)
	}
}

// A same-state update without content or artifact change changes nothing;
// a status message always changes the status time.
func TestUpdateNoOpAndStatus(t *testing.T) {
	c, err := tasks.Update(task(model.TaskWorking), model.TaskUpdate{State: model.TaskWorking}, t1)
	if err != nil || len(c.Changes) != 0 || !c.Task.StatusAt.Equal(t0) {
		t.Fatalf("no-op = %+v, %v", c, err)
	}
	c, err = tasks.Update(task(model.TaskWorking), model.TaskUpdate{StatusMessage: ptr(text("still on it"))}, t1)
	if err != nil || !slices.Equal(c.Changes, []string{model.TaskFieldStatusMessage}) || !c.Task.StatusAt.Equal(t1) || c.State != nil {
		t.Fatalf("status = %+v, %v", c, err)
	}
}

// Reaching canceled clears a pending cancel request.
func TestUpdateCanceledClearsRequest(t *testing.T) {
	cur := task(model.TaskWorking)
	cur.CancelRequested = true
	c, err := tasks.Update(cur, model.TaskUpdate{State: model.TaskCanceled}, t1)
	if err != nil || c.Task.CancelRequested || !slices.Equal(c.Changes, []string{model.TaskFieldState, model.TaskFieldCancelRequested}) {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if !cur.CancelRequested {
		t.Fatal("Update changed its input")
	}
	c, err = tasks.Update(cur, model.TaskUpdate{State: model.TaskCompleted}, t1)
	if err != nil || !c.Task.CancelRequested {
		t.Fatalf("complete keeps the request: %+v, %v", c, err)
	}
}

// Artifacts upsert by ID: replace whole, or append parts; an identical
// replacement changes nothing.
func TestUpsertArtifacts(t *testing.T) {
	cur := task(model.TaskWorking)
	cur.Artifacts = []model.Artifact{{ID: "a", Name: "report", Content: text("p1")}}
	c, err := tasks.Update(cur, model.TaskUpdate{Artifacts: []model.ArtifactUpdate{
		{ID: "a", Content: text("p2"), Append: true, Complete: true, Description: "full"},
		{ID: "b", Content: text("b1")},
	}}, t1)
	if err != nil || !slices.Equal(c.Changes, []string{model.TaskFieldArtifacts}) {
		t.Fatalf("upsert = %+v, %v", c, err)
	}
	a := c.Task.Artifacts
	if len(a) != 2 || a[0].ID != "a" || a[0].Name != "report" || a[0].Description != "full" || !a[0].Complete ||
		len(a[0].Content.Parts) != 2 || a[0].Content.Parts[1].Text != "p2" || a[1].ID != "b" || a[1].Content.Parts[0].Text != "b1" {
		t.Fatalf("artifacts = %+v", a)
	}
	if len(cur.Artifacts[0].Content.Parts) != 1 {
		t.Fatal("Update changed its input's artifacts")
	}
	c, err = tasks.Update(cur, model.TaskUpdate{Artifacts: []model.ArtifactUpdate{{ID: "a", Content: text("x")}}}, t1)
	if err != nil || c.Task.Artifacts[0].Name != "" || c.Task.Artifacts[0].Content.Parts[0].Text != "x" {
		t.Fatalf("replace = %+v, %v", c, err)
	}
	c, err = tasks.Update(cur, model.TaskUpdate{Artifacts: []model.ArtifactUpdate{{ID: "a", Name: "report", Content: text("p1")}}}, t1)
	if err != nil || len(c.Changes) != 0 {
		t.Fatalf("identical replace = %+v, %v", c, err)
	}
}

func TestPayload(t *testing.T) {
	op := model.OperationRef{Tenant: "tenant", ID: "op"}
	m := model.MessageRef{Conversation: conv, ID: "m1"}
	c := tasks.Change{Task: task(model.TaskWorking), Previous: "r0", Changes: []string{model.TaskFieldState}, State: &model.StatusChange{From: "submitted", To: "working"}}
	p := tasks.Payload(c, model.TriggerCommand, &op, &m)
	if p.Task != c.Task.Ref || !slices.Equal(p.Changes, c.Changes) || p.Trigger != model.TriggerCommand || p.State != c.State ||
		p.PreviousRevision != "r0" || p.Operation != &op || p.Message != &m {
		t.Fatalf("payload = %+v", p)
	}
}

func ptr[T any](v T) *T { return &v }

// A remote cancel request marks a live task and changes nothing else; a
// repeat is a no-op and a terminal task refuses it.
func TestCancel(t *testing.T) {
	for _, state := range []string{model.TaskSubmitted, model.TaskWorking, model.TaskInputRequired, model.TaskAuthRequired} {
		c, err := tasks.Cancel(task(state))
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if c.Task.State != state || !c.Task.CancelRequested || !c.Task.StatusAt.Equal(t0) || c.Previous != "r1" ||
			!slices.Equal(c.Changes, []string{model.TaskFieldCancelRequested}) || c.State != nil {
			t.Fatalf("%s: change = %+v", state, c)
		}
		again, err := tasks.Cancel(c.Task)
		if err != nil || len(again.Changes) != 0 || !again.Task.CancelRequested {
			t.Fatalf("%s: repeat = %+v, %v; want no change", state, again, err)
		}
	}
	for _, state := range []string{model.TaskCompleted, model.TaskFailed, model.TaskCanceled, model.TaskRejected} {
		_, err := tasks.Cancel(task(state))
		requireCode(t, err, model.Conflict, state)
	}
}
