package extensions

import (
	"bytes"
	"encoding/json"
	"testing"

	"hop.top/uma/model"
)

// The job hands its stored action to reconciliation, as a copy: an adapter
// that changes what it was given changes nothing stored.
func TestJobHandsItsActionToReconciliation(t *testing.T) {
	action := model.Extension{Name: "com.example.issue.update", Version: 1, Data: json.RawMessage(`{"labels":["bug"]}`)}
	j := &job{intent: intent{Action: action}}
	got := j.RequestedAction()
	if got.Name != action.Name || got.Version != action.Version || !bytes.Equal(got.Data, action.Data) {
		t.Fatalf("RequestedAction = %+v, want %+v", got, action)
	}
	got.Data[2] = 'X'
	if !bytes.Equal(j.intent.Action.Data, action.Data) {
		t.Fatalf("stored action changed to %s through the handed copy", j.intent.Action.Data)
	}
}
