package operations

import (
	"encoding/json"

	"hop.top/uma/model"
)

// Work item acts. A WorkItems.Act update that sets a provider-owned status
// is an outbound operation: its work asks the channel adapter's
// adapter.WorkItemActor to move the provider's work item, and the work item
// changes locally only from the provider's answer.

// IntentWorkItemAct names the stored intent of a provider work item act.
const IntentWorkItemAct = "uma.intent.work_item.act"

// WorkItemActIntent is the durable intent of a provider work item act:
// everything a worker needs to execute it after a restart. The route is
// not part of it: execution reads the work item's stored external
// reference.
type WorkItemActIntent struct {
	WorkItem model.WorkItemRef `json:"work_item"`
	// Status is the requested normalized status.
	Status string `json:"status"`
	// Reason is the requested status reason, empty for the adapter's
	// default.
	Reason string `json:"reason,omitempty"`
}

// Extension encodes the intent.
func (in WorkItemActIntent) Extension() (model.Extension, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return model.Extension{}, model.NewError(model.Permanent, "work item act intent cannot be stored")
	}
	return model.Extension{Name: IntentWorkItemAct, Version: 1, Data: data}, nil
}

// DecodeWorkItemActIntent reads a stored work item act intent.
func DecodeWorkItemActIntent(e model.Extension) (WorkItemActIntent, error) {
	var in WorkItemActIntent
	if e.Name != IntentWorkItemAct || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return WorkItemActIntent{}, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// WorkItemActFingerprint is the version 1 fingerprint of a WorkItems.Act
// request, local or provider path alike: kind, target, action and expected
// revision. The action covers the whole update, status reason included.
func WorkItemActFingerprint(req model.WorkItemAction) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionWorkItemAct))
	f.Set("target", Field().Ref(req.Target.Ref()))
	f.Set("action", Field().Extension(req.Action))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}
