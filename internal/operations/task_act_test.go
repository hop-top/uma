package operations_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// One fingerprint covers every Tasks.Act request: kind, target, action and
// expected revision. An opening's target is its conversation.
func TestTaskActFingerprintFields(t *testing.T) {
	w := actWorkItemRef()
	task := model.TaskRef(w)
	req := model.TaskAction{
		Command: model.Command{ExpectedRevision: "r1"},
		Target:  task,
		Action:  model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: json.RawMessage(`{"state":"working"}`)},
	}
	sum := func(target model.Ref, req model.TaskAction) []byte {
		f := operations.NewFingerprint()
		f.Set("kind", operations.Field().Str(model.ActionTaskAct))
		f.Set("target", operations.Field().Ref(target))
		f.Set("action", operations.Field().Extension(req.Action))
		f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
		return f.Sum()
	}
	if !bytes.Equal(operations.TaskActFingerprint(req), sum(task.Ref(), req)) {
		t.Fatal("update fingerprint field set changed")
	}
	other := req
	other.Correlation = "c-other"
	if !bytes.Equal(operations.TaskActFingerprint(other), sum(task.Ref(), req)) {
		t.Fatal("correlation is covered")
	}
	other.Action.Data = json.RawMessage(`{"state":"working","status_message":{"parts":[{"kind":"text","text":"hi"}]}}`)
	if bytes.Equal(operations.TaskActFingerprint(other), sum(task.Ref(), req)) {
		t.Fatal("status message is not covered")
	}
	other = req
	other.ExpectedRevision = "r2"
	if bytes.Equal(operations.TaskActFingerprint(other), sum(task.Ref(), req)) {
		t.Fatal("expected revision is not covered")
	}

	open := model.TaskAction{
		Target: model.TaskRef{Conversation: task.Conversation},
		Action: model.Extension{Name: model.SchemaTaskOpen, Version: 1, Data: json.RawMessage(`{"message":{}}`)},
	}
	if got := operations.TaskActTarget(open); got != task.Conversation.Ref() {
		t.Fatalf("opening target = %v", got)
	}
	if !bytes.Equal(operations.TaskActFingerprint(open), sum(task.Conversation.Ref(), open)) {
		t.Fatal("opening fingerprint field set changed")
	}
	elsewhere := open
	elsewhere.Target.Conversation.ID = "00000000-0000-7000-8000-000000000009"
	if bytes.Equal(operations.TaskActFingerprint(elsewhere), operations.TaskActFingerprint(open)) {
		t.Fatal("opening conversation is not covered")
	}
}
