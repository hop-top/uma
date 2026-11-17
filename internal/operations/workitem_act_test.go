package operations_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

func actWorkItemRef() model.WorkItemRef {
	inbox := model.InboxRef{Tenant: tenantA, ID: "00000000-0000-7000-8000-000000000001"}
	conv := model.ConversationRef{Inbox: inbox, ID: "00000000-0000-7000-8000-000000000002"}
	return model.WorkItemRef{Conversation: conv, ID: "00000000-0000-7000-8000-000000000003"}
}

// The stored intent of a provider work item act round-trips under its
// name and version; any other name or version is a schema mismatch.
func TestWorkItemActIntentRoundTrip(t *testing.T) {
	in := operations.WorkItemActIntent{WorkItem: actWorkItemRef(), Status: model.WorkItemClosed, Reason: "not_planned"}
	ext, err := in.Extension()
	if err != nil {
		t.Fatal(err)
	}
	if operations.IntentWorkItemAct != "uma.intent.work_item.act" || ext.Name != operations.IntentWorkItemAct || ext.Version != 1 {
		t.Fatalf("intent = %s v%d", ext.Name, ext.Version)
	}
	got, err := operations.DecodeWorkItemActIntent(ext)
	if err != nil || got != in {
		t.Fatalf("decoded %+v, %v; want %+v", got, err, in)
	}
	def, err := operations.WorkItemActIntent{WorkItem: actWorkItemRef(), Status: model.WorkItemOpen}.Extension()
	if err != nil || bytes.Contains(def.Data, []byte("reason")) {
		t.Fatalf("default reason encoded: %s, %v", def.Data, err)
	}
	for name, e := range map[string]model.Extension{
		"other name":    {Name: operations.IntentReply, Version: 1, Data: ext.Data},
		"other version": {Name: operations.IntentWorkItemAct, Version: 2, Data: ext.Data},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := operations.DecodeWorkItemActIntent(e)
			requireCode(t, err, model.SchemaMismatch, "unknown operation intent")
		})
	}
	_, err = operations.DecodeWorkItemActIntent(model.Extension{Name: operations.IntentWorkItemAct, Version: 1, Data: json.RawMessage(`[`)})
	requireCode(t, err, model.Permanent, "unreadable")
}

// One fingerprint covers every WorkItems.Act request, whichever path it
// takes: kind, target, action and expected revision.
func TestWorkItemActFingerprintFields(t *testing.T) {
	req := model.WorkItemAction{
		Command: model.Command{ExpectedRevision: "r1"},
		Target:  actWorkItemRef(),
		Action:  model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: json.RawMessage(`{"status":"closed"}`)},
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionWorkItemAct))
	f.Set("target", operations.Field().Ref(req.Target.Ref()))
	f.Set("action", operations.Field().Extension(req.Action))
	f.Set("expected_revision", operations.Field().Str("r1"))
	if got := operations.WorkItemActFingerprint(req); !bytes.Equal(got, f.Sum()) {
		t.Fatal("fingerprint field set changed")
	}
	other := req
	other.Correlation = "c-other"
	if !bytes.Equal(operations.WorkItemActFingerprint(other), f.Sum()) {
		t.Fatal("correlation is covered")
	}
	other.Action.Data = json.RawMessage(`{"status":"closed","status_reason":"not_planned"}`)
	if bytes.Equal(operations.WorkItemActFingerprint(other), f.Sum()) {
		t.Fatal("status reason is not covered")
	}
}
