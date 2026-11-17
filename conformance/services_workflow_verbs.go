package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// workflowVerbCases run local workflow state and queued provider verbs on
// one conversation of the mutation env: a provider-applied membership
// change leaves the assignee, the snooze and the links as they were, and
// the operation list finds each kind of both families by its key.
var workflowVerbCases = []struct {
	name string
	run  func(*testing.T, *mutationEnv)
}{
	{"MembershipKeepsWorkflowState", workflowMembershipKeepsState},
	{"OperationKindsAcrossFamilies", workflowVerbOperationKinds},
}

// workflowState assigns conv to assignee, snoozes it for a day and links it
// to far, returning each acceptance in that order.
func (env *mutationEnv) workflowState(conv, far model.ConversationRef, assignee model.ActorID) []model.Acceptance {
	env.t.Helper()
	cv := env.client.Conversations()
	assigned, err := cv.Assign(env.ctx, model.ConversationAssignment{Command: env.cmd(""), Conversation: conv, Assignee: assignee})
	if err != nil {
		env.t.Fatalf("Assign: %v", err)
	}
	until := env.now().Add(24 * time.Hour)
	snoozed, err := cv.Snooze(env.ctx, model.ConversationSnooze{Command: env.cmd(""), Conversation: conv, Until: &until})
	if err != nil {
		env.t.Fatalf("Snooze: %v", err)
	}
	linked, err := cv.Link(env.ctx, model.ConversationLinkChange{Command: env.cmd(""), Conversation: conv, Link: model.ConversationLink{Conversation: &far}})
	if err != nil {
		env.t.Fatalf("Link: %v", err)
	}
	return []model.Acceptance{assigned, snoozed, linked}
}

// wantWorkflow requires conv to show assignee, a snooze in effect and the
// one link to far, and the workflow filters to find it in its inbox.
func (env *mutationEnv) wantWorkflow(conv, far model.ConversationRef, assignee model.ActorID) model.Conversation {
	env.t.Helper()
	got := env.conversation(conv)
	if got.Assignee == nil || *got.Assignee != assignee || got.Snooze == nil ||
		len(got.Links) != 1 || got.Links[0].Conversation == nil || *got.Links[0].Conversation != far {
		env.t.Fatalf("conversation %+v, want assigned to %s, snoozed and linked to %v", got, assignee, far)
	}
	yes := true
	f := model.Filter{
		Inboxes: []model.InboxRef{conv.Inbox}, Assignee: &model.AssigneeFilter{Actor: assignee}, Snoozed: &yes, Linked: &yes,
	}
	pg, err := env.client.Conversations().List(env.ctx, serviceList(f, 10, ""))
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Ref != conv {
		env.t.Fatalf("List by assignee, snooze and link = %+v, %v; want the conversation", pg, err)
	}
	return got
}

// workflowMembershipKeepsState: an assigned, snoozed and linked
// conversation gains and loses a participant through the provider; each
// applied change moves the revision and touches only the participants: the
// assignee, the snooze (a membership change is no inbound message) and the
// link stay, in the record and in the stored filters.
func workflowMembershipKeepsState(t *testing.T, env *mutationEnv) {
	conv, far := env.target.Ref.Conversation, env.plain.Ref.Conversation
	env.workflowState(conv, far, "desk-1")
	before := env.wantWorkflow(conv, far, "desk-1")
	ada := env.identity(conv, "ada")
	convs := env.client.Conversations()

	add, err := convs.AddParticipant(env.ctx, env.participantChange(conv, ada.Ref, before.Revision))
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	if done := env.deliver(add.Operation.Ref); done.State != model.ProviderAccepted {
		t.Fatalf("added participant = %+v, want provider_accepted", done)
	}
	added := env.wantWorkflow(conv, far, "desk-1")
	if !hasParticipant(added, ada.Ref) || added.Revision == before.Revision {
		t.Fatalf("conversation after the add = %+v, want ada a participant at a new revision", added)
	}

	rm, err := convs.RemoveParticipant(env.ctx, env.participantChange(conv, ada.Ref, added.Revision))
	if err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	if done := env.deliver(rm.Operation.Ref); done.State != model.ProviderAccepted {
		t.Fatalf("removed participant = %+v, want provider_accepted", done)
	}
	removed := env.wantWorkflow(conv, far, "desk-1")
	if hasParticipant(removed, ada.Ref) || removed.Revision == added.Revision {
		t.Fatalf("conversation after the removal = %+v, want ada gone at a new revision", removed)
	}
}

// workflowVerbOperationKinds: workflow commands and queued provider verbs
// on one conversation each carry the key their command claimed, and
// Operations.List finds each by its kind, all of them together, and by
// their sender.
func workflowVerbOperationKinds(t *testing.T, env *mutationEnv) {
	conv, far := env.target.Ref.Conversation, env.plain.Ref.Conversation
	accepted := env.workflowState(conv, far, "desk-1")
	ada := env.identity(conv, "ada")
	add, err := env.client.Conversations().AddParticipant(env.ctx, env.participantChange(conv, ada.Ref, ""))
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	receipt, err := env.client.Messages().Receipt(env.ctx, env.receipt(env.target.Ref, model.ReceiptRead))
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	accepted = append(accepted, add, receipt)
	kinds := []string{
		model.ActionConversationAssign, model.ActionConversationSnooze, model.ActionConversationLink,
		model.ActionConversationMembership, model.ActionMessageReceipt,
	}
	targets := []model.Ref{conv.Ref(), conv.Ref(), conv.Ref(), conv.Ref(), env.target.Ref.Ref()}
	list := func(f model.Filter) []model.Operation {
		t.Helper()
		pg, err := env.client.Operations().List(env.ctx, serviceList(f, 100, ""))
		if err != nil || !pg.Complete {
			t.Fatalf("List %+v = %+v, %v", f, pg, err)
		}
		return pg.Items
	}
	for i, a := range accepted {
		want := model.OperationKey{Caller: mutationActor, Kind: kinds[i], Target: targets[i], ID: a.Operation.Key.ID}
		if k := a.Operation.Key; k == nil || *k != want || k.ID == "" {
			t.Fatalf("%s accepted with key %+v, want %+v", kinds[i], a.Operation.Key, want)
		}
		got := list(model.Filter{Kinds: []string{kinds[i]}})
		if len(got) != 1 || got[0].Ref != a.Operation.Ref || got[0].Key == nil || *got[0].Key != want {
			t.Fatalf("List by kind %s = %+v, want the one operation with key %+v", kinds[i], got, want)
		}
	}
	if got := list(model.Filter{Kinds: kinds}); len(got) != len(kinds) {
		t.Fatalf("List by every kind holds %d operations, want %d", len(got), len(kinds))
	}
	actor := mutationActor
	if got := list(model.Filter{Kinds: kinds, Actor: &actor}); len(got) != len(kinds) {
		t.Fatalf("List by every kind and the sender holds %d operations, want %d", len(got), len(kinds))
	}
	other := model.ActorID("desk-2")
	if got := list(model.Filter{Kinds: kinds, Actor: &other}); len(got) != 0 {
		t.Fatalf("List by another sender = %+v, want none", got)
	}
}
