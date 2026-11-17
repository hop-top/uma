package services_test

import (
	"testing"

	"hop.top/uma/model"
)

// Act refuses, before any write, a provider-owned status mixed with a
// local field and a status reason on a local status; a provider-owned
// status alone is unsupported on a channel whose adapter does not declare
// work_item_act.
func TestWorkItemActOwnershipRules(t *testing.T) {
	s := newSvc(t)
	provider := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen, model.WorkItemFieldSubject)
	local := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen, model.WorkItemFieldStatus)
	conv := s.stored(provider.Ref).Ref.Conversation
	events := len(s.Events())
	for _, c := range []struct {
		name string
		w    model.WorkItem
		u    model.WorkItemUpdate
		code model.ErrorCode
		want string
	}{
		{"mixed", provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed), Subject: ptr("Renamed")},
			model.Validation, "send provider-owned and uma-owned fields in separate actions"},
		{"reason on local status", local, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved), StatusReason: ptr("completed")},
			model.Validation, "status_reason"},
		{"reason without status", local, model.WorkItemUpdate{Subject: ptr("Renamed"), StatusReason: ptr("completed")},
			model.Validation, "status_reason"},
		{"provider status", provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed), StatusReason: ptr("not_planned")},
			model.Unsupported, "does not declare work_item_act"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.act(c.w, model.OperationID("op-"+c.name), c.w.Revision, c.u)
			requireCode(t, err, c.code, c.want)
			if got := s.stored(c.w.Ref); got.Revision != c.w.Revision || got.Status != c.w.Status {
				t.Fatalf("work item written: %+v", got)
			}
		})
	}
	if len(s.Events()) != events {
		t.Fatal("events emitted")
	}
	got, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: conv})
	if err != nil || got.Status != model.ConversationOpen {
		t.Fatalf("conversation = %+v, %v; want it untouched", got, err)
	}
}
