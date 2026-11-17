package model

import "time"

// Builtin event payloads, version 1 (Event.Payload.Name "uma." + Kind,
// Event.Payload.Version PayloadVersion1).
//
// A payload is references plus a change summary: never message bodies,
// attachment bytes or field values. Consumers read content through the
// authorized service Get, so redaction, audience changes and per-read
// authorization apply to the read, not to a copy in the event log. The
// envelope (Event) already carries ID, kind, tenant, inbox, resource, actor,
// times, correlation, causation, audience and the resource's new revision;
// payloads do not repeat them.
//
// Trigger is one of TriggerProvider, TriggerCommand or TriggerSystem.
// Operation is set whenever Trigger is TriggerCommand. Enum-valued fields
// (Trigger, Origin, Change, Changes, states) are open within version 1: a
// consumer treats an unknown value as "something changed, re-read the
// resource", never as an error, and ignores unknown fields.
//
// Optional references are pointers and absent when unset. Other optional
// fields follow the package wire rule and encode their zero value.

// MessageReceivedPayload is the message.received payload: an inbound
// provider message newly committed to a conversation. uma-authored messages
// emit message.created instead.
type MessageReceivedPayload struct {
	Message MessageRef `json:"message"`
	// Trigger is always TriggerProvider in version 1.
	Trigger string `json:"trigger"`
	// Source is the channel type that produced the message.
	Source TypeKey `json:"source"`
	// Author is the resolved sender identity, when one exists.
	Author *IdentityRef `json:"author,omitempty"`
	// AuthorKind is the message's Author.Kind: a contact kind, or
	// AuthorKindSystem for a system actor, so a consumer can skip bots or
	// agents without reading the message. An addition within version 1: a
	// consumer treats it empty or absent as unknown and reads the message.
	AuthorKind string `json:"author_kind"`
	// InReplyTo is the message the provider reports this one replies to,
	// when uma holds it in the same conversation; absent otherwise.
	InReplyTo *MessageRef `json:"in_reply_to,omitempty"`
	// ConversationCreated reports that this message created the conversation;
	// a conversation.changed event with change "created" is also emitted.
	ConversationCreated bool `json:"conversation_created"`
}

// MessageCreatedPayload is the message.created payload: a message authored
// through uma (Messages.Reply, Messages.AddInternalNote, Conversations.Start
// or the status message of a Tasks.Act update), emitted once at commit,
// before any provider delivery.
type MessageCreatedPayload struct {
	Message MessageRef `json:"message"`
	// Trigger is always TriggerCommand in version 1.
	Trigger string `json:"trigger"`
	// Origin is MessageOriginReply, MessageOriginNote, MessageOriginStart
	// or MessageOriginTaskStatus, MessageOriginResponse; the set is open
	// within version 1.
	Origin    string       `json:"origin"`
	Operation OperationRef `json:"operation"`
	// Source is the channel type that will carry the message; empty for
	// internal notes.
	Source    TypeKey     `json:"source"`
	InReplyTo *MessageRef `json:"in_reply_to,omitempty"`
	// ConversationCreated reports that the start recording this message
	// created its conversation; a conversation.changed event with change
	// "created" is also emitted.
	ConversationCreated bool `json:"conversation_created"`
	// InResponseTo is the interaction the message answers (origin
	// response) or correlates with (a reply naming one).
	InResponseTo *InteractionRef `json:"in_response_to,omitempty"`
}

// MessageUpdatedPayload is the message.updated payload. It has no content
// field: the edited and edit_superseded changes carry the message
// reference, revisions, the operation and the change kind only, and a
// reaction change its PayloadReaction summary; consumers read the content
// through Messages.Get.
//
// A command-triggered change (Messages.Edit, Delete or React applied after
// the provider accepted it) has Trigger TriggerCommand, Operation set and
// Event.Actor the caller. edit_superseded reports an edit the provider
// answered but uma did not apply because the message moved on since the
// command was accepted; it proves nothing about the provider's state.
type MessageUpdatedPayload struct {
	Message MessageRef `json:"message"`
	// Change is one of the MessageChange values.
	Change  string `json:"change"`
	Trigger string `json:"trigger"`
	// PreviousRevision is the message revision before the change; the
	// envelope Revision is the new one. For edit_superseded, which writes
	// nothing, it is the current revision.
	PreviousRevision Revision `json:"previous_revision"`
	// SourceRevision is the provider source version when provider-triggered.
	SourceRevision string `json:"source_revision"`
	// Reaction is set for the reaction_added and reaction_removed changes.
	Reaction  *PayloadReaction `json:"reaction,omitempty"`
	Operation *OperationRef    `json:"operation,omitempty"`
}

// PayloadReaction summarizes a reaction change. Emoji is the change
// summary, not message content.
//
// Version 1 changed in place while uma is unreleased: By became optional
// and Count was added.
type PayloadReaction struct {
	Emoji string `json:"emoji"`
	// By is the provider identity that reacted. It is absent for a
	// command-triggered change, whose reactor is Event.Actor, and for a
	// count-only change.
	By *IdentityRef `json:"by,omitempty"`
	// Count is the emoji's new provider total for a count-only change;
	// zero otherwise.
	Count int `json:"count"`
}

// ConversationChangedPayload is the conversation.changed payload.
type ConversationChangedPayload struct {
	Conversation ConversationRef `json:"conversation"`
	// Changes is the non-empty set of ConversationField values that changed.
	Changes []string `json:"changes,omitempty"`
	Trigger string   `json:"trigger"`
	// Status is set when Changes holds ConversationFieldStatus.
	Status *StatusChange `json:"status,omitempty"`
	// PreviousRevision is empty when the conversation was created.
	PreviousRevision Revision      `json:"previous_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
}

// StatusChange is a normalized status transition.
type StatusChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DeliveryUpdatedPayload is the delivery.updated payload: a state transition
// of an operation with remote effects. Purely local commits do not emit it.
type DeliveryUpdatedPayload struct {
	Operation     OperationRef  `json:"operation"`
	State         DeliveryState `json:"state"`
	PreviousState DeliveryState `json:"previous_state"`
	// Result is the generic reference of the resource the operation
	// produced or targets.
	Result *Ref `json:"result,omitempty"`
	// ProviderReferences are the references known so far, also for partial
	// outcomes.
	ProviderReferences []ExternalRef `json:"provider_references,omitempty"`
	// ReadConfirmedAt is Operation.ReadConfirmedAt once set. The event that
	// first carries it is the read confirmation of a delivered operation:
	// State and PreviousState are both delivered.
	ReadConfirmedAt *time.Time `json:"read_confirmed_at,omitempty"`
	// Failure is set for failed, rejected, outcome_unknown, retry_pending
	// and dead_lettered states.
	Failure *DeliveryFailure `json:"failure,omitempty"`
	// Trigger is TriggerProvider (callback, receipt) or TriggerSystem
	// (reconcile, lease expiry).
	Trigger string `json:"trigger"`
}

// DeliveryFailure is the safe summary of a failed delivery.
type DeliveryFailure struct {
	Code        ErrorCode   `json:"code"`
	Retry       RetryAdvice `json:"retry"`
	SafeMessage string      `json:"safe_message"`
}

// WorkItemUpdatedPayload is the work_item.updated payload: creation (import)
// and every local or provider-side change of a work item.
type WorkItemUpdatedPayload struct {
	WorkItem WorkItemRef `json:"work_item"`
	// Changes is the non-empty set of WorkItemField values that changed.
	Changes []string `json:"changes,omitempty"`
	// Trigger is TriggerProvider (synced from the tracker) or TriggerCommand
	// (WorkItems.Act).
	Trigger string `json:"trigger"`
	// Status is set when Changes holds WorkItemFieldStatus.
	Status *WorkItemStatusChange `json:"status,omitempty"`
	// PreviousRevision is empty when the work item was created.
	PreviousRevision Revision      `json:"previous_revision"`
	SourceRevision   string        `json:"source_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
}

// WorkItemStatusChange is a normalized status transition plus the
// provider's raw values.
type WorkItemStatusChange struct {
	From    string `json:"from"`
	To      string `json:"to"`
	RawFrom string `json:"raw_from"`
	RawTo   string `json:"raw_to"`
}

// IdentityLinkedPayload is the identity.linked payload, one per link history
// record of any class. It carries no evidence facts and no other identities.
type IdentityLinkedPayload struct {
	// Link is the link history record ID.
	Link      ID            `json:"link"`
	Class     LinkClass     `json:"class"`
	From      ContactRef    `json:"from"`
	To        ContactRef    `json:"to"`
	Assurance LinkAssurance `json:"assurance"`
}

// Contact change values (ContactUpdatedPayload.Changes).
const (
	ContactChangeDisplayName = "display_name"
	ContactChangeAttributes  = "attributes"
)

// ContactCreatedPayload is the contact.created payload. It names the
// contact, its kind and the operation; the display name and attributes are
// read through Contacts.List under ActionContactRead.
type ContactCreatedPayload struct {
	Contact   ContactRef   `json:"contact"`
	Kind      string       `json:"kind"`
	Trigger   string       `json:"trigger"`
	Operation OperationRef `json:"operation"`
}

// ContactUpdatedPayload is the contact.updated payload. Changes is the
// non-empty set of ContactChange* values that differ; the values themselves
// are not carried. PreviousRevision is the contact revision before the call.
type ContactUpdatedPayload struct {
	Contact          ContactRef   `json:"contact"`
	Changes          []string     `json:"changes,omitempty"`
	Trigger          string       `json:"trigger"`
	PreviousRevision Revision     `json:"previous_revision"`
	Operation        OperationRef `json:"operation"`
}

// ConversationLabelPayload is the conversation.label_added and
// conversation.label_removed payload: one label added to or removed from
// the conversation's labels. It carries the label reference only; the
// label's name is read through Labels.Get under ActionLabelRead. One
// Conversations.AddLabels or RemoveLabels call emits one event per label
// it changed, all at the same new revision.
type ConversationLabelPayload struct {
	Conversation ConversationRef `json:"conversation"`
	Label        LabelRef        `json:"label"`
	// Trigger is TriggerCommand in version 1; TriggerProvider is reserved
	// for provider label sync.
	Trigger string `json:"trigger"`
	// PreviousRevision is the conversation revision before the call.
	PreviousRevision Revision      `json:"previous_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
}

// TaskUpdatedPayload is the task.updated payload: the opening and every
// change of an agent task.
type TaskUpdatedPayload struct {
	Task TaskRef `json:"task"`
	// Changes is the non-empty set of TaskField values that changed;
	// TaskFieldCreated on opening.
	Changes []string `json:"changes,omitempty"`
	// Trigger is TriggerProvider (opened by an ingress task binding) or
	// TriggerCommand (Tasks.Act, a TaskSink cancel request).
	Trigger string `json:"trigger"`
	// State is set when Changes holds TaskFieldState or TaskFieldCreated;
	// From is empty on opening.
	State *StatusChange `json:"state,omitempty"`
	// PreviousRevision is empty when the task was created.
	PreviousRevision Revision      `json:"previous_revision"`
	Operation        *OperationRef `json:"operation,omitempty"`
	// Message is the message the change concerns: the status message an
	// update recorded (Changes holds TaskFieldStatusMessage), or on opening
	// the message that opened the task.
	Message *MessageRef `json:"message,omitempty"`
}

// InteractionReceivedPayload is the interaction.received payload: an
// accepted interaction record. It names identifiers only: never submitted
// fields, arguments, the response token or the respond window.
type InteractionReceivedPayload struct {
	Interaction InteractionRef `json:"interaction"`
	// Trigger is always TriggerProvider in version 1.
	Trigger string `json:"trigger"`
	// Source is the channel type that reported the interaction.
	Source TypeKey `json:"source"`
	// Kind is the interaction kind.
	Kind string `json:"kind"`
	// Actor is the resolved actor identity.
	Actor IdentityRef `json:"actor"`
	// ActorKind is the actor's contact kind (Interaction.Actor.Kind), so
	// a consumer can skip bots or agents without reading the interaction.
	// An addition within version 1: a consumer treats it empty or absent
	// as unknown and reads the interaction.
	ActorKind string `json:"actor_kind"`
	// Action is the provider action ID of an action.
	Action string `json:"action"`
	// Command is the command name of a command; its arguments are never
	// in the payload.
	Command string `json:"command"`
	// Message is the resolved local message the control sits on.
	Message *MessageRef `json:"message,omitempty"`
	// ConversationCreated reports that the interaction created its
	// conversation; a conversation.changed event with change "created" is
	// also emitted.
	ConversationCreated bool `json:"conversation_created"`
}

// InteractionUpdatedPayload is the interaction.updated payload: a change
// of an interaction's response state, or its redaction.
type InteractionUpdatedPayload struct {
	Interaction InteractionRef `json:"interaction"`
	// Change is one of the InteractionChange values.
	Change string `json:"change"`
	// Trigger is always TriggerCommand in version 1: Respond, Ephemeral or
	// Redact.
	Trigger string `json:"trigger"`
	// PreviousRevision is the interaction revision before the change.
	PreviousRevision Revision `json:"previous_revision"`
	// Operation is the Respond, Ephemeral or Redact operation.
	Operation *OperationRef `json:"operation,omitempty"`
}
