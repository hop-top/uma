package model

import "testing"

func TestParticipantChangeValidate(t *testing.T) {
	valid := func() ParticipantChange {
		return ParticipantChange{
			Command: Command{Call: Call{Correlation: "corr-1"}, Operation: "op-1"}, Conversation: convA,
			Identity: IdentityRef{Inbox: convA.Inbox, ID: idA},
		}
	}
	requireOK(t, valid().Validate())
	rev := valid()
	rev.ExpectedRevision = "r1"
	requireOK(t, rev.Validate())
	for _, tc := range []struct {
		name, field string
		mut         func(*ParticipantChange)
	}{
		{"no conversation", "conversation.inbox.tenant", func(r *ParticipantChange) { r.Conversation = ConversationRef{} }},
		{"no identity", "identity.inbox.tenant", func(r *ParticipantChange) { r.Identity = IdentityRef{} }},
		{"identity of another inbox", "identity", func(r *ParticipantChange) { r.Identity.Inbox.ID = idC }},
		{"no operation", "operation", func(r *ParticipantChange) { r.Operation = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mut(&r)
			requireError(t, r.Validate(), Validation, tc.field)
		})
	}
}
