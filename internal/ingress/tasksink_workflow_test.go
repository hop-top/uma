package ingress_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Workflow state is local desk state: the conversation a task sink hands
// an agent adapter carries no assignee or snooze.
func TestTaskSinkContextOmitsWorkflowState(t *testing.T) {
	w := newSinkWorld(t)
	ref := w.a1.Ref.Conversation
	desk := model.ActorID("desk-1")
	if err := w.St.Transact(w.Ctx, ref.Inbox.Tenant, func(tx store.Tx) error {
		c, err := tx.GetConversation(w.Ctx, ref)
		if err != nil {
			return err
		}
		next := c.Clone()
		next.Assignee = &desk
		next.Snooze = &model.Snooze{Until: w.Clock.Now().Add(time.Hour), At: w.Clock.Now(), By: desk}
		next.Revision = c.Revision + "-wf"
		return tx.PutConversation(w.Ctx, next, c.Revision)
	}); err != nil {
		t.Fatal(err)
	}
	conv, err := w.ts.Context(w.Ctx, w.alice(), contextRef(w.harness, "c1"))
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if conv.Ref != ref || conv.Assignee != nil || conv.Snooze != nil {
		t.Fatalf("Context = %+v, want the conversation without workflow state", conv)
	}
}
