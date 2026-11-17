package model

import "time"

// Conversation workflow state: local desk state on a conversation that
// never reaches a provider. It is part of the conversation record and its
// revision (Conversation.Assignee, Conversation.Snooze), changed through
// Conversations.Assign, Unassign, Snooze and Unsnooze, and readable only by
// a principal holding ActionConversationReadWorkflow on the conversation:
// every other reader gets the fields zeroed, so participants limited to a
// conversation's public content never see them.

// Snooze hides a conversation from lists that exclude snoozed ones until
// Until, or until a live inbound message is recorded after At, whichever
// comes first; a message imported from the provider's history
// (Message.ImportedBy) is not news and never wakes it. It is a query-time
// predicate over the stored record: nothing runs at Until, a snooze needs
// no scheduler and survives a restart, and waking emits no event. It is
// independent of Conversation.Status.
type Snooze struct {
	// Until is when the snooze ends.
	Until time.Time `json:"until"`
	// At is when the snooze was set; a live inbound message recorded after
	// it wakes the conversation.
	At time.Time `json:"at"`
	// By is the actor that set it.
	By ActorID `json:"by"`
}

// InEffect reports whether the snooze hides its conversation at now, given
// the recording time of the conversation's newest live inbound message
// that was not imported (nil when it has none): Until is after now and no
// such message was recorded after At. A nil snooze is never in effect.
func (s *Snooze) InEffect(now time.Time, newestInbound *time.Time) bool {
	return s != nil && s.Until.After(now) && (newestInbound == nil || !newestInbound.After(s.At))
}

// AssigneeFilter selects conversations by assignee (Filter.Assignee).
// Exactly one part is set: Actor selects the conversations assigned to
// that actor, Self those assigned to the calling principal (the service
// resolves it to Actor; a store refuses it) and None the unassigned ones.
type AssigneeFilter struct {
	Actor ActorID `json:"actor"`
	Self  bool    `json:"self"`
	None  bool    `json:"none"`
}

// Validate checks that exactly one part is set and Actor is a valid actor.
func (a AssigneeFilter) Validate() error { return a.validate("assignee") }

func (a AssigneeFilter) validate(path string) error {
	set := 0
	for _, on := range []bool{a.Actor != "", a.Self, a.None} {
		if on {
			set++
		}
	}
	if set != 1 {
		return invalidField(path, "set exactly one of actor, self and none")
	}
	if a.Actor != "" {
		return checkToken(fieldPath(path, "actor"), string(a.Actor))
	}
	return nil
}

// Matches reports whether a conversation with assignee matches the filter.
// Self never matches: it must be resolved to Actor first.
func (a AssigneeFilter) Matches(assignee *ActorID) bool {
	switch {
	case a.None:
		return assignee == nil
	case a.Actor != "":
		return assignee != nil && *assignee == a.Actor
	}
	return false
}

// ConversationAssignment assigns a conversation to a desk actor
// (Conversations.Assign: Assignee set) or clears its assignee
// (Conversations.Unassign: Assignee empty). ExpectedRevision is optional
// and, when set, is the conversation's revision.
type ConversationAssignment struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Assignee     ActorID         `json:"assignee"`
}

// RevisionRule is RevisionOptional, as for conversation labels.
func (ConversationAssignment) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the command, the conversation and, when set, the
// assignee. Whether Assignee must be set depends on the call: Assign
// requires it, Unassign refuses it.
func (r ConversationAssignment) Validate() error {
	var assignee error
	if r.Assignee != "" {
		assignee = checkToken("assignee", string(r.Assignee))
	}
	return firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), assignee)
}

// ConversationSnooze snoozes a conversation until Until
// (Conversations.Snooze: Until set) or ends its snooze
// (Conversations.Unsnooze: Until nil). ExpectedRevision is optional and,
// when set, is the conversation's revision.
type ConversationSnooze struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Until        *time.Time      `json:"until,omitempty"`
}

// RevisionRule is RevisionOptional, as for conversation labels.
func (ConversationSnooze) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the command and the conversation. Whether Until must be
// set depends on the call, and whether it is in the future on the engine's
// clock.
func (r ConversationSnooze) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false))
}

// ConversationAssignmentPayload is the conversation.assigned and
// conversation.unassigned payload: references only. Assignee is the new
// assignee (nil when unassigned), Previous the one it replaced (nil when
// there was none). The event log thereby keeps the assignment history.
type ConversationAssignmentPayload struct {
	Conversation ConversationRef `json:"conversation"`
	Assignee     *ActorID        `json:"assignee,omitempty"`
	Previous     *ActorID        `json:"previous,omitempty"`
	// Trigger is TriggerCommand in version 1.
	Trigger string `json:"trigger"`
	// PreviousRevision is the conversation revision before the call.
	PreviousRevision Revision      `json:"previous_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
}

// ConversationSnoozePayload is the conversation.snoozed and
// conversation.unsnoozed payload. Until is set on conversation.snoozed.
// Waking, by expiry or by an inbound message, emits no event.
type ConversationSnoozePayload struct {
	Conversation ConversationRef `json:"conversation"`
	Until        *time.Time      `json:"until,omitempty"`
	// Trigger is TriggerCommand in version 1.
	Trigger string `json:"trigger"`
	// PreviousRevision is the conversation revision before the call.
	PreviousRevision Revision      `json:"previous_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
}

// IsWorkflowEvent reports whether kind is one of the conversation
// workflow events, which a subscriber receives only while it holds
// ActionConversationReadWorkflow on the conversation.
func IsWorkflowEvent(kind string) bool {
	switch kind {
	case EventConversationAssigned, EventConversationUnassigned,
		EventConversationSnoozed, EventConversationUnsnoozed,
		EventConversationLinked, EventConversationUnlinked:
		return true
	}
	return false
}

// WithoutWorkflow returns c with its workflow state zeroed, as a reader
// without ActionConversationReadWorkflow sees it.
func (c Conversation) WithoutWorkflow() Conversation {
	c.Assignee = nil
	c.Snooze = nil
	c.Links = nil
	return c
}
