package a2a_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

func TestGetTaskUnknown(t *testing.T) {
	r := newRig(t)
	r.rpc("agent-1", "GetTask", map[string]any{"id": "nope"}).fail(t, -32001)
	r.rpc("agent-1", "GetTask", map[string]any{}).fail(t, -32602)
}

func TestListTasksPagination(t *testing.T) {
	r := newRig(t)
	first := decode[sendResult](t, r.send("agent-1", "m-0", "t0", immediately()).ok(t)).Task
	ids := []string{first.ID}
	for i := 1; i < 5; i++ {
		r.clock.Advance(time.Second)
		res := decode[sendResult](t, r.send("agent-1", "m-"+itoa(int64(i)), "t", map[string]any{
			"contextId": first.ContextID, "configuration": map[string]any{"returnImmediately": true},
		}).ok(t))
		ids = append(ids, res.Task.ID)
	}
	r.hostUpdate(ids[0], model.TaskUpdate{State: model.TaskCompleted, Artifacts: []model.ArtifactUpdate{{ID: "a", Content: text("out")}}})
	var seen []string
	token := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"contextId": first.ContextID, "pageSize": 2, "includeArtifacts": false}
		if token != "" {
			params["pageToken"] = token
		}
		l := decode[listResult](t, r.rpc("agent-1", "ListTasks", params).ok(t))
		if l.PageSize != 2 || l.NextPageToken == nil || len(l.Tasks) > 2 {
			t.Fatalf("page %d = %+v", page, l)
		}
		for _, tk := range l.Tasks {
			if len(tk.Artifacts) != 0 {
				t.Fatalf("task %s carries artifacts without includeArtifacts", tk.ID)
			}
			seen = append(seen, tk.ID)
		}
		token = *l.NextPageToken
		if token == "" {
			break
		}
	}
	if token != "" || len(seen) != 5 {
		t.Fatalf("paged %d tasks, final token %q; want 5 and an empty token", len(seen), token)
	}
	// Most recent status first: the completed task (updated last) leads.
	if seen[0] != ids[0] {
		t.Fatalf("order = %v, want %s (latest status change) first", seen, ids[0])
	}
	// Filters and artifacts.
	l := decode[listResult](t, r.rpc("agent-1", "ListTasks", map[string]any{
		"contextId": first.ContextID, "status": "TASK_STATE_COMPLETED", "includeArtifacts": true,
	}).ok(t))
	if len(l.Tasks) != 1 || l.Tasks[0].ID != ids[0] || len(l.Tasks[0].Artifacts) != 1 {
		t.Fatalf("completed = %+v", l.Tasks)
	}
	if l.NextPageToken == nil || *l.NextPageToken != "" {
		t.Fatalf("final page token = %v, want an empty string", l.NextPageToken)
	}
	later := time.Unix(1_700_000_000, 0).UTC().Add(3500 * time.Millisecond).Format(time.RFC3339Nano)
	l = decode[listResult](t, r.rpc("agent-1", "ListTasks", map[string]any{
		"contextId": first.ContextID, "statusTimestampAfter": later,
	}).ok(t))
	if len(l.Tasks) != 2 || l.Tasks[0].ID != ids[0] || l.Tasks[1].ID != ids[4] {
		t.Fatalf("after %s = %+v, want the two latest status changes", later, l.Tasks)
	}
	for name, params := range map[string]map[string]any{
		"page size 0":   {"pageSize": 0},
		"page size 101": {"pageSize": 101},
		"bad status":    {"status": "DONE"},
		"bad timestamp": {"statusTimestampAfter": "yesterday"},
		"bad token":     {"contextId": first.ContextID, "pageToken": "garbage"},
	} {
		t.Run(name, func(t *testing.T) { r.rpc("agent-1", "ListTasks", params).fail(t, -32602) })
	}
}

func TestCancelTask(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "go", immediately()).ok(t)).Task
	r.hostUpdate(a.ID, model.TaskUpdate{State: model.TaskWorking})
	for i := 0; i < 2; i++ { // a repeated request is a no-op
		got := decode[task](t, r.rpc("agent-1", "CancelTask", map[string]any{"id": a.ID}).ok(t))
		if got.ID != a.ID || got.Status.State != "TASK_STATE_WORKING" {
			t.Fatalf("CancelTask = %+v, want the task in its current state", got)
		}
	}
	if !r.taskByWireID(a.ID).CancelRequested {
		t.Fatal("cancel request not recorded")
	}
	r.hostUpdate(a.ID, model.TaskUpdate{State: model.TaskCanceled})
	e := r.rpc("agent-1", "CancelTask", map[string]any{"id": a.ID}).fail(t, -32002)
	if info := e.info(t); info.Reason != "TASK_NOT_CANCELABLE" {
		t.Fatalf("detail = %+v", info)
	}
	r.rpc("agent-1", "CancelTask", map[string]any{}).fail(t, -32602)
}

func TestCancelTaskTerminal(t *testing.T) {
	r := newRig(t)
	a := decode[sendResult](t, r.send("agent-1", "m-1", "go", immediately()).ok(t)).Task
	r.hostUpdate(a.ID, model.TaskUpdate{State: model.TaskCompleted})
	r.rpc("agent-1", "CancelTask", map[string]any{"id": a.ID}).fail(t, -32002)
	if r.taskByWireID(a.ID).CancelRequested {
		t.Fatal("a request on a terminal task was recorded")
	}
}
