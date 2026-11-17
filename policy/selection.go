package policy

import "hop.top/uma/model"

// EventClass classifies the new inbound message or interaction a selection
// decides for.
type EventClass string

const (
	// EventContactMessage is a message authored by the resolved contact.
	EventContactMessage EventClass = "contact_message"
	// EventAutoMessage is a message the adapter marks automated. It never
	// reopens a conversation.
	EventAutoMessage EventClass = "auto_message"
	// EventOutboundStart is the first message of an outbound-first
	// conversation (Conversations.Start), sent by uma to the Sender
	// identity, which here names the recipient. The built-in selectors
	// treat it as an automated message: it never reopens a conversation.
	// SelectionInput.Operation names the start.
	EventOutboundStart EventClass = "outbound_start"
	// EventInteraction is a structured interaction (model.EnvelopeInteraction)
	// whose source message did not place it in a conversation. The
	// built-in selectors treat it as a contact message.
	EventInteraction EventClass = "interaction"
)

// Threading headers a ReferenceMatch names.
const (
	HeaderInReplyTo  = "in-reply-to"
	HeaderReferences = "references"
)

// ReferenceMatch is one stored message of the inbox that a threading header
// names: Header is HeaderInReplyTo or HeaderReferences, MsgID the matched
// msg-id and Conversation the conversation storing it. Core tries msg-ids in
// precedence order, the In-Reply-To msg-id first, then References entries
// from last to first, and stops at the first one stored in the inbox: every
// match names that msg-id, one per conversation storing it, so more than one
// match is duplicate evidence.
type ReferenceMatch struct {
	Header       string
	MsgID        string
	Conversation model.ConversationRef
}
