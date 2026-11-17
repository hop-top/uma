package model

// ParticipantChange adds an identity to a conversation's participants at
// the provider (Conversations.AddParticipant) or removes it
// (Conversations.RemoveParticipant). Identity is an identity of the
// conversation's inbox. ExpectedRevision is optional and compared with the
// conversation's revision when set.
type ParticipantChange struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Identity     IdentityRef     `json:"identity"`
}

// RevisionRule is RevisionOptional: a caller may change membership
// without reading the conversation first.
func (ParticipantChange) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the command, the conversation and the identity, which
// must belong to the conversation's inbox.
func (r ParticipantChange) Validate() error {
	if err := firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Conversation.validate("conversation", false),
		r.Identity.validate("identity", false),
	); err != nil {
		return err
	}
	if r.Identity.Inbox != r.Conversation.Inbox {
		return invalidField("identity", "must belong to the conversation's inbox")
	}
	return nil
}
