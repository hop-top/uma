package model

import (
	"slices"
	"strconv"
	"time"
)

// MaxConversationLinks caps the links one conversation holds.
const MaxConversationLinks = 64

// ConversationLink is one link a conversation holds, named by its far end:
// another conversation of the tenant, or a work item. A link is
// association only: it merges no history, changes no reply scope and is
// never a provider effect. A link between two conversations is held by
// both, written in one unit; a link to a work item is held by the
// conversation alone. Exactly one of Conversation and WorkItem is set.
type ConversationLink struct {
	Conversation *ConversationRef `json:"conversation,omitempty"`
	WorkItem     *WorkItemRef     `json:"work_item,omitempty"`
}

// Validate checks that exactly one end is set and valid.
func (l ConversationLink) Validate() error { return l.validate("link") }

func (l ConversationLink) validate(path string) error {
	switch {
	case (l.Conversation == nil) == (l.WorkItem == nil):
		return invalidField(path, "set exactly one of conversation and work_item")
	case l.Conversation != nil:
		return l.Conversation.validate(fieldPath(path, "conversation"), false)
	}
	return l.WorkItem.validate(fieldPath(path, "work_item"), false)
}

// Ref returns the generic reference of the far end.
func (l ConversationLink) Ref() Ref {
	if l.Conversation != nil {
		return l.Conversation.Ref()
	}
	if l.WorkItem != nil {
		return l.WorkItem.Ref()
	}
	return Ref{}
}

// CompareConversationLinks orders links canonically: conversations before
// work items, then by inbox, conversation and ID.
func CompareConversationLinks(a, b ConversationLink) int {
	key := func(l ConversationLink) []string {
		if c := l.Conversation; c != nil {
			return []string{"0", string(c.Inbox.ID), string(c.ID), ""}
		}
		w := l.WorkItem
		return []string{"1", string(w.Conversation.Inbox.ID), string(w.Conversation.ID), string(w.ID)}
	}
	return slices.Compare(key(a), key(b))
}

// ValidateLinks checks a conversation's links: at most
// MaxConversationLinks, each valid, in its tenant, not the conversation
// itself, sorted by CompareConversationLinks without duplicates.
func (c Conversation) ValidateLinks() error {
	if len(c.Links) > MaxConversationLinks {
		return invalidField("links", "exceeds "+strconv.Itoa(MaxConversationLinks)+" links")
	}
	for i, l := range c.Links {
		field := "links[" + strconv.Itoa(i) + "]"
		if err := l.validate(field); err != nil {
			return err
		}
		if l.Ref().Tenant != c.Ref.Inbox.Tenant {
			return invalidField(field, "must belong to the conversation's tenant")
		}
		if l.Conversation != nil && *l.Conversation == c.Ref {
			return invalidField(field, "must not link the conversation to itself")
		}
		if i > 0 && CompareConversationLinks(c.Links[i-1], l) >= 0 {
			return invalidField(field, "must be sorted without duplicates")
		}
	}
	return nil
}

// Clone returns a deep copy.
func (l ConversationLink) Clone() ConversationLink {
	l.Conversation = clonePtr(l.Conversation, same)
	l.WorkItem = clonePtr(l.WorkItem, same)
	return l
}

// ConversationLinkChange links a conversation to a far end
// (Conversations.Link) or removes that link (Conversations.Unlink).
// ExpectedRevision is optional and, when set, is the revision of
// Conversation.
type ConversationLinkChange struct {
	Command
	Conversation ConversationRef  `json:"conversation"`
	Link         ConversationLink `json:"link"`
}

// RevisionRule is RevisionOptional, as for conversation labels.
func (ConversationLinkChange) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the command, both ends, that they share a tenant and
// that a conversation is not linked to itself.
func (r ConversationLinkChange) Validate() error {
	if err := firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), r.Link.validate("link")); err != nil {
		return err
	}
	if r.Link.Ref().Tenant != r.Conversation.Inbox.Tenant {
		return invalidField("link", "must belong to the conversation's tenant")
	}
	if r.Link.Conversation != nil && *r.Link.Conversation == r.Conversation {
		return invalidField("link.conversation", "must not be the conversation itself")
	}
	return nil
}

// ConversationLinkPayload is the conversation.linked and
// conversation.unlinked payload, emitted once per conversation end:
// Conversation is the end the event is about and Link its far end.
// References only.
type ConversationLinkPayload struct {
	Conversation ConversationRef  `json:"conversation"`
	Link         ConversationLink `json:"link"`
	// Trigger is TriggerCommand in version 1.
	Trigger string `json:"trigger"`
	// PreviousRevision is the revision of Conversation before the call.
	PreviousRevision Revision      `json:"previous_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
}

// ReadMarker is one actor's read state of one conversation: messages
// recorded at or before ReadThrough are read for that actor. It is its own
// record with its own revision: marking never moves the conversation's
// revision. Each actual move emits a conversation.read_marked or
// conversation.unread_marked event (ReadMarkerPayload), delivered to the actor
// alone. Only the actor reads it.
type ReadMarker struct {
	Conversation ConversationRef `json:"conversation"`
	Actor        ActorID         `json:"actor"`
	ReadThrough  time.Time       `json:"read_through"`
	UpdatedAt    time.Time       `json:"updated_at"`
	Revision     Revision        `json:"revision"`
}

// Validate checks the conversation, the actor and the revision.
func (m ReadMarker) Validate() error {
	var rev error
	if m.Revision == "" {
		rev = invalidField("revision", "required")
	}
	return firstError(m.Conversation.validate("conversation", false), checkToken("actor", string(m.Actor)), rev)
}

// Unread reports whether a message counts as unread for the marker's
// actor: recorded after ReadThrough, not an echo of a uma-sent message,
// not authored by the actor through uma and not imported from the
// provider's history (Message.ImportedBy). A nil marker has read nothing.
func (m *ReadMarker) Unread(msg Message, actor ActorID) bool {
	if msg.EchoOf != nil || msg.ImportedBy != "" || (msg.Origin != nil && msg.Origin.Actor == actor) {
		return false
	}
	return m == nil || msg.RecordedAt.After(m.ReadThrough)
}

// ReadMark moves the caller's read marker on a conversation
// (Conversations.MarkRead, MarkUnread). Message, when set, names a message
// of the conversation; see those methods for its meaning.
type ReadMark struct {
	Call
	Conversation ConversationRef `json:"conversation"`
	Message      *MessageRef     `json:"message,omitempty"`
}

// Validate checks the call, the conversation and that Message, when set,
// is in it.
func (r ReadMark) Validate() error {
	if err := firstError(r.Call.Validate(), r.Conversation.validate("conversation", false)); err != nil {
		return err
	}
	if r.Message != nil {
		if err := r.Message.validate("message", false); err != nil {
			return err
		}
		if r.Message.Conversation != r.Conversation {
			return invalidField("message", "must be in the conversation")
		}
	}
	return nil
}

// UnreadFilter selects the conversations holding a message unread for
// Actor (ReadMarker.Unread; a conversation the actor never marked is
// unread when it holds such a message). The service fills Actor with the
// calling principal and refuses any other; a store requires it.
type UnreadFilter struct {
	Actor ActorID `json:"actor"`
}

func (u UnreadFilter) validate(path string) error {
	if u.Actor == "" {
		return nil
	}
	return checkToken(fieldPath(path, "actor"), string(u.Actor))
}

// ReadMarkerPayload is the conversation.read_marked and conversation.unread_marked
// payload, emitted once per actual move of Actor's read marker: references
// only, never a message or its content. Revision is the marker's new
// revision, the one Conversations.ReadMarker then returns, so a client
// ordering them by it keeps the latest. The event's own Revision is the
// conversation's, which marking never moves.
type ReadMarkerPayload struct {
	Conversation ConversationRef `json:"conversation"`
	Actor        ActorID         `json:"actor"`
	Revision     Revision        `json:"revision"`
}

// IsReadMarkerEvent reports whether kind is conversation.read_marked or
// conversation.unread_marked, which a subscriber receives only while it holds
// ActionConversationReadWorkflow on the conversation and is the marker's
// own actor.
func IsReadMarkerEvent(kind string) bool {
	return kind == EventConversationReadMarked || kind == EventConversationUnreadMarked
}
