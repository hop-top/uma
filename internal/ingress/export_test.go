package ingress

import "hop.top/uma/model"

// Internals that tests in package ingress_test exercise directly.

const (
	ChangeCreated       = changeCreated
	ChangeDeleted       = changeDeleted
	ChangeReactionAdded = changeReactionAdded
	MaxCreateKeyBytes   = maxCreateKeyBytes
	RoleContact         = roleContact
	RoleParticipant     = roleParticipant
)

type (
	ContactSelector            = contactSelector
	ScopedSelector             = scopedSelector
	ConversationChangedPayload = conversationChangedPayload
	MessageReceivedPayload     = messageReceivedPayload
	MessageUpdatedPayload      = model.MessageUpdatedPayload
)

var (
	CheckSelection = checkSelection
	ContactGroup   = contactGroup
	IsParticipant  = isParticipant
	SelectorError  = selectorError
	ClaimsSender   = claimsSender
)
