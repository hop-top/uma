package privacy_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// Workflow state is about desk actors, not the subject: an export of a
// conversation that holds an assignee and a snooze carries neither.
func TestExportOmitsWorkflowState(t *testing.T) {
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}))
	if _, err := r.C.Conversations().Assign(r.Ctx, model.ConversationAssignment{
		Command: cmd("op-assign", ""), Conversation: r.Conv.Ref, Assignee: "desk-1",
	}); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	until := r.Clock.Now().Add(time.Hour)
	if _, err := r.C.Conversations().Snooze(r.Ctx, model.ConversationSnooze{
		Command: cmd("op-snooze", ""), Conversation: r.Conv.Ref, Until: &until,
	}); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	_, _, lines := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	convs := ofType[model.Conversation](t, lines, model.ExportLineConversation)
	if len(convs) != 1 || convs[0].Assignee != nil || convs[0].Snooze != nil {
		t.Fatalf("conversation lines = %+v, want one without workflow state", convs)
	}
}
