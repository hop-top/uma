package model

// ReadAction returns the policy action that grants reading a resource of
// kind k. ok is false for kinds no read action covers: events about them
// are never delivered and held operations on them never shown.
func ReadAction(k ResourceKind) (string, bool) {
	switch k {
	case KindChannel:
		return ActionChannelRead, true
	case KindInbox:
		return ActionInboxRead, true
	case KindLabel:
		return ActionLabelRead, true
	case KindContact:
		return ActionContactRead, true
	case KindIdentity:
		return ActionIdentityRead, true
	case KindConversation:
		return ActionConversationRead, true
	case KindMessage:
		return ActionMessageRead, true
	case KindOperation:
		return ActionOperationRead, true
	case KindTask:
		return ActionTaskRead, true
	case KindWorkItem:
		return ActionWorkItemRead, true
	case KindAttachment:
		return ActionAttachmentRead, true
	case KindStream:
		return ActionStreamRead, true
	case KindInteraction:
		return ActionInteractionRead, true
	}
	return "", false
}
