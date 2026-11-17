package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// request is one JSON-RPC call of the walkthrough.
type request struct {
	Title  string
	Method string
	Params any
}

func sendParams(id, text string, returnImmediately bool) map[string]any {
	params := map[string]any{"message": map[string]any{
		"messageId": id, "role": "ROLE_USER", "parts": []map[string]any{{"text": text}},
	}}
	if returnImmediately {
		params["configuration"] = map[string]any{"returnImmediately": true}
	}
	return params
}

// runnableRequests are the walkthrough's calls that need nothing from an
// earlier answer. The test sends each of them, so the printed commands work.
func runnableRequests() []request {
	return []request{
		{"Message-only answer (blocking): the reply comes back as a Message", "SendMessage", sendParams("demo-1", "echo hello", false)},
		{"A task (blocking): completed, with an artifact and a status message", "SendMessage", sendParams("demo-2", "report Q3", false)},
		{"A task, answered at once: submitted; the executor carries on", "SendMessage", sendParams("demo-3", "report Q4", true)},
		{"A task the blocking call gives up on: -32603 DEADLINE_EXCEEDED after the wait; the task still completes", "SendMessage", sendParams("demo-4", "late Q1", false)},
		{"A task held working until a cancel request, answered at once", "SendMessage", sendParams("demo-5", "hold", true)},
		{"Your tasks, most recent first", "ListTasks", map[string]any{}},
	}
}

// printCommands writes the curl command of each walkthrough request and the
// two that name an ID taken from an earlier answer.
func printCommands(w io.Writer, endpoint, token string) {
	_, _ = fmt.Fprintf(w, "Agent card (no credentials):\n  curl -s %sagent-card.json\n\n", endpoint)
	reqs := runnableRequests()
	reqs = append(reqs,
		request{"One task with its history (replace TASK_ID with an id from an answer above)", "GetTask", map[string]any{"id": "TASK_ID", "historyLength": 10}},
		request{"Ask the host to cancel a task (try the held one)", "CancelTask", map[string]any{"id": "TASK_ID"}},
	)
	for _, r := range reqs {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": r.Method, "params": r.Params})
		_, _ = fmt.Fprintf(w, "%s:\n  curl -s %s \\\n    -H 'Authorization: Bearer %s' -H 'A2A-Version: 1.0' -H 'Content-Type: application/json' \\\n    -d '%s'\n\n",
			r.Title, endpoint, token, strings.ReplaceAll(string(body), "'", `'\''`))
	}
}
