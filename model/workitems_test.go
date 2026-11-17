package model

import (
	"encoding/json"
	"strings"
	"testing"
)

var (
	chanA     = ChannelRef{Tenant: tenantA, ID: idA}
	issueRef2 = ExternalRef{Channel: chanA, Namespace: "com.example.tracker", Kind: RefKindWorkItem, ID: "issue-2"}
	userRef   = ExternalRef{Channel: chanA, Namespace: "com.example.user", Kind: "user", ID: "u1"}
)

func TestWorkItemStateValidate(t *testing.T) {
	ok := WorkItemState{
		Subject: "Broken build", URL: "https://tracker.example/1", Status: WorkItemOpen, RawStatus: "triage",
		Requester: userRef, Assignees: []ExternalRef{userRef},
		Fields:      []Extension{{Name: "com.example.tracker.priority", Version: 1, Data: json.RawMessage(`"p1"`)}},
		LocalFields: []string{WorkItemFieldStatus},
	}
	requireOK(t, ok.Validate())
	requireOK(t, WorkItemState{}.Validate())
	requireOK(t, WorkItemState{RawStatus: "unmapped"}.Validate())
	merged := WorkItemState{Binding: WorkItemBindingMerged, Successor: &issueRef2}
	requireOK(t, merged.Validate())

	cases := map[string]struct {
		s     WorkItemState
		field string
	}{
		"unknown status":         {WorkItemState{Status: "triage"}, "work_item.status"},
		"unknown binding":        {WorkItemState{Binding: "archived"}, "work_item.binding"},
		"merged no successor":    {WorkItemState{Binding: WorkItemBindingMerged}, "work_item.successor"},
		"moved no successor":     {WorkItemState{Binding: WorkItemBindingMoved}, "work_item.successor"},
		"active with successor":  {WorkItemState{Successor: &issueRef2}, "work_item.successor"},
		"deleted with successor": {WorkItemState{Binding: WorkItemBindingDeleted, Successor: &issueRef2}, "work_item.successor"},
		"successor not item":     {WorkItemState{Binding: WorkItemBindingMoved, Successor: &userRef}, "work_item.successor.kind"},
		"bad requester":          {WorkItemState{Requester: ExternalRef{Channel: chanA, Kind: "user", ID: "u"}}, "work_item.requester.namespace"},
		"bad assignee":           {WorkItemState{Assignees: []ExternalRef{{}}}, "work_item.assignees[0].channel.tenant"},
		"bad field":              {WorkItemState{Fields: []Extension{{Name: "x"}}}, "work_item.fields[0].name"},
		"local requester":        {WorkItemState{LocalFields: []string{WorkItemFieldRequester}}, "work_item.local_fields[0]"},
		"duplicate local":        {WorkItemState{LocalFields: []string{WorkItemFieldStatus, WorkItemFieldStatus}}, "work_item.local_fields[1]"},
		"subject with NUL":       {WorkItemState{Subject: "a\x00b"}, "work_item.subject"},
		"oversized subject":      {WorkItemState{Subject: strings.Repeat("s", maxWorkItemTextBytes+1)}, "work_item.subject"},
		"oversized raw status":   {WorkItemState{RawStatus: strings.Repeat("s", maxTokenBytes+1)}, "work_item.raw_status"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) { requireError(t, c.s.Validate(), Validation, c.field) })
	}
}

func TestDecodeWorkItemUpdate(t *testing.T) {
	act := func(name string, v uint32, data string) Extension {
		return Extension{Name: name, Version: v, Data: json.RawMessage(data)}
	}
	u, err := DecodeWorkItemUpdate(act(SchemaWorkItemUpdate, 1, `{"status":"resolved","assignees":[]}`))
	requireOK(t, err)
	if u.Status == nil || *u.Status != WorkItemResolved || u.Assignees == nil || len(*u.Assignees) != 0 || u.Subject != nil || u.Fields != nil {
		t.Fatalf("update = %+v", u)
	}
	if got := u.Changed(); strings.Join(got, ",") != "status,assignees" {
		t.Fatalf("Changed = %v", got)
	}
	_, err = DecodeWorkItemUpdate(act("com.example.tracker.close", 1, `{}`))
	requireError(t, err, Unsupported, "action.name")
	_, err = DecodeWorkItemUpdate(act(SchemaWorkItemUpdate, 2, `{"status":"open"}`))
	requireError(t, err, SchemaMismatch, "action.version")
	for name, data := range map[string]string{
		"unknown field": `{"status":"open","priority":"p1"}`,
		"trailing data": `{"status":"open"} {}`,
		"not an object": `"open"`,
		"no data":       ``,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeWorkItemUpdate(act(SchemaWorkItemUpdate, 1, data))
			requireError(t, err, Validation, "action.data")
		})
	}
	_, err = DecodeWorkItemUpdate(act(SchemaWorkItemUpdate, 1, `{}`))
	requireError(t, err, Validation, "action.data")
	_, err = DecodeWorkItemUpdate(act(SchemaWorkItemUpdate, 1, `{"status":"done"}`))
	requireError(t, err, Validation, "action.data.status")
	_, err = DecodeWorkItemUpdate(act(SchemaWorkItemUpdate, 1, `{"assignees":[{"kind":"user"}]}`))
	requireError(t, err, Validation, "action.data.assignees[0].channel.tenant")
}

func TestEchoRef(t *testing.T) {
	op := OperationRef{Tenant: tenantA, ID: idB}
	r := EchoRef(chanA, op, idC)
	requireOK(t, r.Validate())
	if r.Namespace != EchoNamespace || r.Kind != RefKindAttempt || r.ID != string(idC) || r.ParentID != string(idB) || r.Channel != chanA {
		t.Fatalf("EchoRef = %+v", r)
	}
}

func TestEnvelopeWorkItemClonesState(t *testing.T) {
	env := Envelope{WorkItem: &WorkItemState{Assignees: []ExternalRef{userRef}}}
	c := env.Clone()
	c.WorkItem.Assignees[0].ID = "other"
	if env.WorkItem.Assignees[0].ID != "u1" || c.WorkItem == env.WorkItem {
		t.Fatal("Envelope.Clone aliases the work item state")
	}
}

// A status reason travels only with a status, as one lowercase segment of
// at most 64 bytes; absent means the adapter's default reason.
func TestWorkItemUpdateStatusReasonValidate(t *testing.T) {
	act := func(data string) Extension {
		return Extension{Name: SchemaWorkItemUpdate, Version: 1, Data: json.RawMessage(data)}
	}
	u, err := DecodeWorkItemUpdate(act(`{"status":"closed","status_reason":"not_planned"}`))
	requireOK(t, err)
	if u.StatusReason == nil || *u.StatusReason != "not_planned" {
		t.Fatalf("update = %+v", u)
	}
	if got := u.Changed(); strings.Join(got, ",") != "status" {
		t.Fatalf("Changed = %v, want the reason counted with status", got)
	}
	reason := func(s string) *string { return &s }
	open := WorkItemOpen
	requireOK(t, WorkItemUpdate{Status: &open, StatusReason: reason("reopened")}.Validate())
	requireOK(t, WorkItemUpdate{Status: &open, StatusReason: reason(strings.Repeat("r", MaxStatusReasonBytes))}.Validate())
	for name, c := range map[string]struct {
		u     WorkItemUpdate
		field string
	}{
		"without status": {WorkItemUpdate{StatusReason: reason("completed")}, "action.data.status_reason"},
		"empty":          {WorkItemUpdate{Status: &open, StatusReason: reason("")}, "action.data.status_reason"},
		"uppercase":      {WorkItemUpdate{Status: &open, StatusReason: reason("Completed")}, "action.data.status_reason"},
		"spaces":         {WorkItemUpdate{Status: &open, StatusReason: reason("not planned")}, "action.data.status_reason"},
		"dotted":         {WorkItemUpdate{Status: &open, StatusReason: reason("com.example.done")}, "action.data.status_reason"},
		"leading digit":  {WorkItemUpdate{Status: &open, StatusReason: reason("2nd")}, "action.data.status_reason"},
		"oversized":      {WorkItemUpdate{Status: &open, StatusReason: reason(strings.Repeat("r", MaxStatusReasonBytes+1))}, "action.data.status_reason"},
	} {
		t.Run(name, func(t *testing.T) { requireError(t, c.u.Validate(), Validation, c.field) })
	}
	requireOK(t, ValidateStatusReason("not_planned"))
	requireError(t, ValidateStatusReason("Not_Planned"), Validation, "status_reason")
}
