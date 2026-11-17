package a2a_test

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// normalize replaces the server-allocated IDs of a result by the
// placeholders the fixtures use.
func normalize(t *testing.T, raw json.RawMessage, replace map[string]string) string {
	t.Helper()
	s := string(raw)
	for from, to := range replace {
		if from == "" || !strings.Contains(s, from) {
			t.Fatalf("result lacks %q: %s", from, s)
		}
		s = strings.ReplaceAll(s, from, to)
	}
	return canonical(t, []byte(s))
}

// Specification section 6.1: a question answered by a completed task with
// one artifact. The fixture is the specification's response, with the
// timestamp the fixed test clock yields; only the IDs the server allocates
// are normalized.
func TestGoldenBasicTaskExecution(t *testing.T) {
	r := newRig(t)
	done := async(func() reply { return r.send("agent-1", "msg-uuid", "What is the weather today?", nil) })
	conv, msgs := r.waitMessages("", 1)
	r.hostOpen(conv.Ref, msgs[0], model.TaskWorking)
	id := r.taskIDs()[0]
	r.hostUpdate(id, model.TaskUpdate{State: model.TaskCompleted, Artifacts: []model.ArtifactUpdate{{
		ID: "artifact-uuid", Name: "Weather Report", Content: text("Today will be sunny with a high of 75°F"),
	}}})
	res := await(t, done).ok(t)
	got := normalize(t, res, map[string]string{id: "task-uuid", conv.External[0].ID: "context-uuid"})
	if want := canonical(t, golden(t, "spec_6_1_task.json")); got != want {
		t.Fatalf("result:\n got %s\nwant %s", got, want)
	}
}

// Specification section 6.3: a task that needs more input answers with
// input-required and the agent's question, and the follow-up message names
// the task.
func TestGoldenMultiTurnInteraction(t *testing.T) {
	r := newRig(t)
	done := async(func() reply { return r.send("agent-1", "msg-1", "Book me a flight", nil) })
	conv, msgs := r.waitMessages("", 1)
	r.hostOpen(conv.Ref, msgs[0], model.TaskWorking)
	id := r.taskIDs()[0]
	question := text("I need more details. Where would you like to fly from and to?")
	r.hostUpdate(id, model.TaskUpdate{State: model.TaskInputRequired, StatusMessage: &question})
	res := await(t, done).ok(t)
	statusID := decode[sendResult](t, res).Task.Status.Message.MessageID
	got := normalize(t, res, map[string]string{id: "task-uuid", conv.External[0].ID: "context-uuid", statusID: "message-uuid"})
	if want := canonical(t, golden(t, "spec_6_3_input_required.json")); got != want {
		t.Fatalf("result:\n got %s\nwant %s", got, want)
	}

	follow := async(func() reply {
		return r.send("agent-1", "msg-2", "From San Francisco to New York", map[string]any{"taskId": id})
	})
	// The conversation holds the request, the agent's question and now the
	// follow-up.
	_, all := r.waitMessages("", 3)
	if last := all[2]; last.Task == nil || last.Task.ID != r.taskByWireID(id).Ref.ID || last.Origin != nil {
		t.Fatalf("follow-up %+v is not the agent's message bound to the task", last)
	}
	r.hostUpdate(id, model.TaskUpdate{State: model.TaskCompleted})
	final := decode[sendResult](t, await(t, follow).ok(t))
	if final.Task == nil || final.Task.ID != id || final.Task.Status.State != "TASK_STATE_COMPLETED" || final.Task.ContextID != conv.External[0].ID {
		t.Fatalf("follow-up result = %+v", final)
	}
}
