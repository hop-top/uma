package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func taskAct(name string, v uint32, data string) Extension {
	return Extension{Name: name, Version: v, Data: json.RawMessage(data)}
}

const openMsgA = `{"message":{"conversation":{"inbox":{"tenant":"tenant-a","id":"00000000-0000-7000-8000-000000000001"},"id":"00000000-0000-7000-8000-000000000002"},"id":"00000000-0000-7000-8000-000000000003"},"state":""}`

func TestDecodeTaskAction(t *testing.T) {
	d, err := DecodeTaskAction(taskAct(SchemaTaskOpen, 1, openMsgA))
	requireOK(t, err)
	if d.Open == nil || d.Update != nil || d.Open.Message != msgA || d.Open.State != "" {
		t.Fatalf("open = %+v", d)
	}
	d, err = DecodeTaskAction(taskAct(SchemaTaskUpdate, 1,
		`{"state":"input_required","status_message":{"parts":[{"kind":"text","text":"Please provide the document"}]},`+
			`"audience":{"visibility":"public","policy_ref":""},`+
			`"artifacts":[{"id":"a1","name":"n","description":"d","content":{"parts":[{"kind":"text","text":"x"}]},"append":false,"complete":true}]}`))
	requireOK(t, err)
	if d.Update == nil || d.Open != nil || d.Update.State != TaskInputRequired || d.Update.StatusMessage == nil ||
		d.Update.Audience == nil || len(d.Update.Artifacts) != 1 || !d.Update.Artifacts[0].Complete {
		t.Fatalf("update = %+v", d)
	}
	_, err = DecodeTaskAction(taskAct("com.example.agent.retry", 1, `{}`))
	requireError(t, err, Unsupported, "action.name")
	_, err = DecodeTaskAction(taskAct(SchemaWorkItemUpdate, 1, `{"status":"open"}`))
	requireError(t, err, Unsupported, "action.name")
	_, err = DecodeTaskAction(taskAct(SchemaTaskUpdate, 2, `{"state":"working"}`))
	requireError(t, err, SchemaMismatch, "action.version")
	_, err = DecodeTaskAction(taskAct(SchemaTaskOpen, 2, openMsgA))
	requireError(t, err, SchemaMismatch, "action.version")

	for name, c := range map[string]struct {
		name, data, field string
	}{
		"update unknown field":   {SchemaTaskUpdate, `{"state":"working","role":"agent"}`, "action.data"},
		"open unknown field":     {SchemaTaskOpen, `{"message":{},"task":"t"}`, "action.data"},
		"update no data":         {SchemaTaskUpdate, ``, "action.data"},
		"open no data":           {SchemaTaskOpen, ``, "action.data"},
		"update trailing data":   {SchemaTaskUpdate, `{"state":"working"} {}`, "action.data"},
		"update not an object":   {SchemaTaskUpdate, `"working"`, "action.data"},
		"update sets nothing":    {SchemaTaskUpdate, `{}`, "action.data"},
		"update empty artifacts": {SchemaTaskUpdate, `{"artifacts":[]}`, "action.data"},
		"update empty state":     {SchemaTaskUpdate, `{"state":""}`, "action.data"},
		"update undeclared":      {SchemaTaskUpdate, `{"state":"done"}`, "action.data.state"},
		"update unspecified":     {SchemaTaskUpdate, `{"state":"unspecified"}`, "action.data.state"},
		"audience no status msg": {SchemaTaskUpdate, `{"state":"working","audience":{"visibility":"internal","policy_ref":""}}`, "action.data.audience"},
		"bad audience":           {SchemaTaskUpdate, `{"status_message":{"parts":[{"kind":"text","text":"x"}]},"audience":{"visibility":"secret","policy_ref":""}}`, "action.data.audience.visibility"},
		"empty status message":   {SchemaTaskUpdate, `{"status_message":{}}`, "action.data.status_message.parts"},
		"artifact without id":    {SchemaTaskUpdate, `{"artifacts":[{"id":"","content":{"parts":[{"kind":"text","text":"x"}]}}]}`, "action.data.artifacts[0].id"},
		"artifact duplicate id":  {SchemaTaskUpdate, `{"artifacts":[{"id":"a","content":{"parts":[{"kind":"text","text":"x"}]}},{"id":"a","content":{"parts":[{"kind":"text","text":"y"}]}}]}`, "action.data.artifacts[1].id"},
		"artifact no content":    {SchemaTaskUpdate, `{"artifacts":[{"id":"a"}]}`, "action.data.artifacts[0].content.parts"},
		"artifact long name":     {SchemaTaskUpdate, `{"artifacts":[{"id":"a","name":"` + strings.Repeat("n", maxTaskTextBytes+1) + `","append":true}]}`, "action.data.artifacts[0].name"},
		"open no message":        {SchemaTaskOpen, `{"state":"working"}`, "action.data.message"},
		"open undeclared state":  {SchemaTaskOpen, strings.Replace(openMsgA, `"state":""`, `"state":"done"`, 1), "action.data.state"},
		"open unspecified state": {SchemaTaskOpen, strings.Replace(openMsgA, `"state":""`, `"state":"unspecified"`, 1), "action.data.state"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeTaskAction(taskAct(c.name, 1, c.data))
			requireError(t, err, Validation, c.field)
		})
	}

	// Appending a final empty chunk only closes the artifact.
	d, err = DecodeTaskAction(taskAct(SchemaTaskUpdate, 1, `{"artifacts":[{"id":"a","append":true,"complete":true}]}`))
	requireOK(t, err)
	if len(d.Update.Artifacts) != 1 || !d.Update.Artifacts[0].Append {
		t.Fatalf("update = %+v", d.Update)
	}
	// A status message alone is a same-state update.
	_, err = DecodeTaskAction(taskAct(SchemaTaskUpdate, 1, `{"status_message":{"parts":[{"kind":"text","text":"x"}]}}`))
	requireOK(t, err)
}

func TestTaskActionOpenTargetScope(t *testing.T) {
	open := taskAct(SchemaTaskOpen, 1, openMsgA)
	update := taskAct(SchemaTaskUpdate, 1, `{"state":"working"}`)
	scope := TaskRef{Conversation: convA}
	runRequestCases(t, map[string]requestCase{
		"open allocates":         {TaskAction{Command: cmd, Target: scope, Action: open}, ""},
		"open names a task":      {TaskAction{Command: cmd, Target: task, Action: open}, "target.id"},
		"open without scope":     {TaskAction{Command: cmd, Action: open}, "target.conversation.inbox.tenant"},
		"open with revision":     {TaskAction{Command: cmdRev, Target: scope, Action: open}, "expected_revision"},
		"update names a task":    {TaskAction{Command: cmdRev, Target: task, Action: update}, ""},
		"update empty target ID": {TaskAction{Command: cmdRev, Target: scope, Action: update}, "target.id"},
		"update needs revision":  {TaskAction{Command: cmd, Target: task, Action: update}, "expected_revision"},
		"other empty target ID":  {TaskAction{Command: cmd, Target: scope, Action: ext}, "target.id"},
	})
	if (TaskAction{Action: open}).RevisionRule() != RevisionAbsent ||
		(TaskAction{Action: update}).RevisionRule() != RevisionRequired ||
		(TaskAction{Action: ext}).RevisionRule() != RevisionOptional {
		t.Fatal("task action revision rules")
	}
}

func TestTaskActionDecode(t *testing.T) {
	scope := TaskRef{Conversation: convA}
	d, err := TaskAction{Command: cmd, Target: scope, Action: taskAct(SchemaTaskOpen, 1, openMsgA)}.Decode()
	requireOK(t, err)
	if d.Open == nil || d.Open.Message != msgA {
		t.Fatalf("open = %+v", d)
	}
	other := TaskRef{Conversation: convB}
	_, err = TaskAction{Command: cmd, Target: other, Action: taskAct(SchemaTaskOpen, 1, openMsgA)}.Decode()
	requireError(t, err, Validation, "action.data.message")

	foreign := `{"parts":[{"kind":"attachment","attachment":{"inbox":{"tenant":"tenant-a","id":"00000000-0000-7000-8000-000000000004"},"id":"00000000-0000-7000-8000-000000000004"}}]}`
	_, err = TaskAction{Command: cmdRev, Target: task, Action: taskAct(SchemaTaskUpdate, 1, `{"status_message":`+foreign+`}`)}.Decode()
	requireError(t, err, Validation, "action.data.status_message.parts[0].attachment")
	_, err = TaskAction{Command: cmdRev, Target: task, Action: taskAct(SchemaTaskUpdate, 1, `{"artifacts":[{"id":"a","content":`+foreign+`}]}`)}.Decode()
	requireError(t, err, Validation, "action.data.artifacts[0].content.parts[0].attachment")
	_, err = TaskAction{Command: cmdRev, Target: task, Action: taskAct("com.example.agent.retry", 1, `{}`)}.Decode()
	requireError(t, err, Unsupported, "action.name")
}
