package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wire types: only the fields the tests read.
type (
	rpcReply struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int               `json:"code"`
			Data []json.RawMessage `json:"data"`
		} `json:"error"`
	}
	wireMessage struct {
		ContextID string `json:"contextId"`
		TaskID    string `json:"taskId"`
		Role      string `json:"role"`
		Parts     []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	wireTask struct {
		ID        string `json:"id"`
		ContextID string `json:"contextId"`
		Status    struct {
			State   string       `json:"state"`
			Message *wireMessage `json:"message"`
		} `json:"status"`
		Artifacts []struct {
			ArtifactID string `json:"artifactId"`
			Parts      []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"artifacts"`
		History []wireMessage `json:"history"`
	}
	sendResult struct {
		Task    *wireTask    `json:"task"`
		Message *wireMessage `json:"message"`
	}
	listResult struct {
		Tasks []wireTask `json:"tasks"`
	}
)

const (
	tokenOne = "token-one"
	tokenTwo = "token-two"
)

// rig is one host served over httptest.
type rig struct {
	t    *testing.T
	srv  *httptest.Server
	host *host
}

// newRig starts the host and its executor; the server is created first so
// the card can name the URL the host is mounted at.
func newRig(t *testing.T, o options) *rig {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	o.PublicURL = "http://" + srv.Listener.Addr().String() + mountPath
	o.Tokens = map[string]string{tokenOne: "agent-one", tokenTwo: "agent-two"}
	if o.BlockingWait == 0 {
		o.BlockingWait = 5 * time.Second
	}
	o.PollInterval = 50 * time.Millisecond
	h, err := newHost(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = h.Handler()
	srv.Start()
	ran := make(chan error, 1)
	runCtx, stop := context.WithCancel(t.Context())
	go func() { ran <- h.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-ran; err != nil {
			t.Errorf("Run: %v", err)
		}
		srv.Close()
		if err := h.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &rig{t: t, srv: srv, host: h}
}

// post sends one JSON-RPC request as the holder of token.
func (r *rig) post(token, method string, params any) (*http.Response, []byte) {
	r.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		r.t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, r.srv.URL+mountPath, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return res, b
}

// call sends a request that must succeed and decodes its result into out.
func call[T any](r *rig, token, method string, params any) T {
	r.t.Helper()
	res, b := r.post(token, method, params)
	var rp rpcReply
	if res.StatusCode != http.StatusOK || json.Unmarshal(b, &rp) != nil || rp.Error != nil {
		r.t.Fatalf("%s = HTTP %d: %s", method, res.StatusCode, b)
	}
	var out T
	if err := json.Unmarshal(rp.Result, &out); err != nil {
		r.t.Fatalf("%s result %s: %v", method, rp.Result, err)
	}
	return out
}

// fail sends a request that must fail with JSON-RPC code.
func (r *rig) fail(token, method string, params any, code int) *struct {
	Code int               `json:"code"`
	Data []json.RawMessage `json:"data"`
} {
	r.t.Helper()
	res, b := r.post(token, method, params)
	var rp rpcReply
	if res.StatusCode != http.StatusOK || json.Unmarshal(b, &rp) != nil || rp.Error == nil || rp.Error.Code != code {
		r.t.Fatalf("%s: want error %d, got HTTP %d: %s", method, code, res.StatusCode, b)
	}
	return rp.Error
}

func send(id, text string, extra map[string]any) map[string]any {
	msg := map[string]any{"messageId": id, "role": "ROLE_USER", "parts": []map[string]any{{"text": text}}}
	params := map[string]any{"message": msg}
	for k, v := range extra {
		if k == "configuration" {
			params[k] = v
		} else {
			msg[k] = v
		}
	}
	return params
}

var immediately = map[string]any{"configuration": map[string]any{"returnImmediately": true}}

// waitState polls GetTask until the task reaches state.
func (r *rig) waitState(token, id, state string) wireTask {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		t := call[wireTask](r, token, "GetTask", map[string]any{"id": id, "historyLength": 10})
		if t.Status.State == state {
			return t
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("task %s is %s, want %s", id, t.Status.State, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCardAndAuthentication(t *testing.T) {
	r := newRig(t, options{})
	res, err := r.srv.Client().Get(r.srv.URL + mountPath + "agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	card, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(card), r.srv.URL+mountPath) {
		t.Fatalf("card = %d %s; want the card without credentials, naming %s", res.StatusCode, card, r.srv.URL+mountPath)
	}
	for _, token := range []string{"", "wrong"} {
		res, _ := r.post(token, "ListTasks", map[string]any{})
		if res.StatusCode != http.StatusUnauthorized || !strings.Contains(res.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("token %q = HTTP %d, WWW-Authenticate %q; want 401 naming Bearer", token, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
}

// A message the host answers without a task stays message-only: the
// blocking call returns the reply as a Message and no task exists.
func TestBlockingMessageOnly(t *testing.T) {
	r := newRig(t, options{})
	got := call[sendResult](r, tokenOne, "SendMessage", send("m-echo", "echo hello", nil))
	if got.Task != nil || got.Message == nil || len(got.Message.Parts) != 1 || got.Message.Parts[0].Text != "echo: hello" {
		t.Fatalf("echo = %+v, want the message-only reply", got)
	}
	if got.Message.ContextID == "" || got.Message.TaskID != "" {
		t.Fatalf("reply = %+v, want a context and no task", got.Message)
	}
	if l := call[listResult](r, tokenOne, "ListTasks", map[string]any{}); len(l.Tasks) != 0 {
		t.Fatalf("tasks = %+v, want none: no task is invented for a message-only exchange", l.Tasks)
	}
}

// The host opens a task for a report: the blocking call returns it
// completed, with the artifact and the one status message.
func TestBlockingTask(t *testing.T) {
	r := newRig(t, options{})
	got := call[sendResult](r, tokenOne, "SendMessage", send("m-report", "report Q3", map[string]any{
		"configuration": map[string]any{"historyLength": 10},
	}))
	task := got.Task
	if task == nil || task.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("report = %+v, want a completed task", got)
	}
	if len(task.Artifacts) != 1 || task.Artifacts[0].ArtifactID != "report" || !strings.Contains(task.Artifacts[0].Parts[0].Text, "Q3") {
		t.Fatalf("artifacts = %+v, want the Q3 report", task.Artifacts)
	}
	if m := task.Status.Message; m == nil || len(m.Parts) != 1 || !strings.Contains(m.Parts[0].Text, "Q3") {
		t.Fatalf("status message = %+v, want the content of the completing update", m)
	}
	// History: the request and exactly one status message.
	if len(task.History) != 2 {
		t.Fatalf("history = %+v, want the request and one status message", task.History)
	}
	if again := call[wireTask](r, tokenOne, "GetTask", map[string]any{"id": task.ID, "historyLength": 10}); len(again.History) != 2 {
		t.Fatalf("GetTask history = %+v", again.History)
	}
	if l := call[listResult](r, tokenOne, "ListTasks", map[string]any{"contextId": task.ContextID}); len(l.Tasks) != 1 || l.Tasks[0].ID != task.ID {
		t.Fatalf("ListTasks = %+v, want the one task", l.Tasks)
	}
}

// returnImmediately opens the task with the message and answers at once;
// the executor continues the bound task.
func TestReturnImmediately(t *testing.T) {
	r := newRig(t, options{})
	got := call[sendResult](r, tokenOne, "SendMessage", send("m-now", "report Q4", immediately))
	if got.Task == nil || got.Task.Status.State != "TASK_STATE_SUBMITTED" {
		t.Fatalf("returnImmediately = %+v, want the submitted task", got)
	}
	done := r.waitState(tokenOne, got.Task.ID, "TASK_STATE_COMPLETED")
	if len(done.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", done.Artifacts)
	}
}

// A wait that runs out is -32603 DEADLINE_EXCEEDED, not a task, and
// cancels nothing: the task the host opens later is readable, and
// resending the message returns the first context.
func TestBlockingExpiry(t *testing.T) {
	r := newRig(t, options{BlockingWait: 300 * time.Millisecond, LateDelay: time.Second})
	e := r.fail(tokenOne, "SendMessage", send("m-late", "late Q1", nil), -32603)
	if len(e.Data) != 1 {
		t.Fatalf("error data = %s", e.Data)
	}
	var info struct {
		Reason   string            `json:"reason"`
		Domain   string            `json:"domain"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(e.Data[0], &info); err != nil {
		t.Fatal(err)
	}
	if info.Reason != "DEADLINE_EXCEEDED" || info.Domain != "uma.hop.top" {
		t.Fatalf("info = %+v, want DEADLINE_EXCEEDED in uma.hop.top", info)
	}
	contextID := info.Metadata["contextId"]
	if contextID == "" || info.Metadata["messageId"] != "m-late" || info.Metadata["taskId"] != "" {
		t.Fatalf("metadata = %v, want the context and message, no task yet", info.Metadata)
	}

	// The host opens the task after the caller gave up.
	var task wireTask
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if l := call[listResult](r, tokenOne, "ListTasks", map[string]any{"contextId": contextID}); len(l.Tasks) == 1 {
			task = l.Tasks[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task was never opened")
		}
	}
	r.waitState(tokenOne, task.ID, "TASK_STATE_COMPLETED")

	// The retry recovers the first context and creates nothing.
	retry := call[sendResult](r, tokenOne, "SendMessage", send("m-late", "late Q1", nil))
	if retry.Task == nil || retry.Task.ContextID != contextID || retry.Task.ID != task.ID {
		t.Fatalf("retry = %+v, want task %s in context %s", retry, task.ID, contextID)
	}
	if l := call[listResult](r, tokenOne, "ListTasks", map[string]any{"contextId": contextID}); len(l.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want the retry to create none", l.Tasks)
	}
}

// CancelTask records a request; the host decides, and here honors it.
func TestCancel(t *testing.T) {
	r := newRig(t, options{})
	got := call[sendResult](r, tokenOne, "SendMessage", send("m-hold", "hold", immediately))
	id := got.Task.ID
	r.waitState(tokenOne, id, "TASK_STATE_WORKING")
	r.fail(tokenTwo, "CancelTask", map[string]any{"id": id}, -32001) // another agent's task
	call[wireTask](r, tokenOne, "CancelTask", map[string]any{"id": id})
	r.waitState(tokenOne, id, "TASK_STATE_CANCELED")
	r.fail(tokenOne, "CancelTask", map[string]any{"id": id}, -32002) // terminal
}

// Tasks belong to the agent whose token created them.
func TestAgentsSeeOnlyTheirOwn(t *testing.T) {
	r := newRig(t, options{})
	got := call[sendResult](r, tokenOne, "SendMessage", send("m-own", "report Q2", nil))
	r.fail(tokenTwo, "GetTask", map[string]any{"id": got.Task.ID}, -32001)
	if l := call[listResult](r, tokenTwo, "ListTasks", map[string]any{}); len(l.Tasks) != 0 {
		t.Fatalf("agent two lists %+v, want none", l.Tasks)
	}
}

// The requests printed for curl are real: each runs against the host.
func TestPrintedRequestsWork(t *testing.T) {
	r := newRig(t, options{})
	for _, ex := range runnableRequests() {
		res, b := r.post(tokenOne, ex.Method, ex.Params)
		var rp rpcReply
		if res.StatusCode != http.StatusOK || json.Unmarshal(b, &rp) != nil || rp.Error != nil {
			t.Errorf("%s: HTTP %d: %s", ex.Title, res.StatusCode, b)
		}
	}
}
