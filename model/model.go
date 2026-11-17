// Package model holds uma domain values, requests, results, references, events
// and errors. It imports only the standard library.
//
// References are identifiers, never authorization. Typed refs carry their full
// ownership chain (tenant, inbox, conversation); constructors and Validate
// reject malformed or unscoped refs, and CheckScope rejects refs owned by
// another tenant. Values holding slices, maps or byte buffers are snapshots:
// Clone returns a copy that shares no mutable memory with the original.
//
// Wire and storage form is encoding/json with explicit snake_case keys.
// Pointer, slice, map and raw JSON fields are omitempty (nil or empty means
// absent, never null); every other field is always encoded. Durations are
// integer milliseconds under an _ms key.
package model

import (
	"encoding/json"
	"time"
)

// Principal is supplied by trusted host authentication, never decoded from a request body.
type Principal struct {
	Tenant TenantID `json:"tenant"`
	Actor  ActorID  `json:"actor"`
}

type Call struct {
	Correlation CorrelationID `json:"correlation"`
}
type Command struct {
	Call
	Operation        OperationID `json:"operation"`
	ExpectedRevision Revision    `json:"expected_revision"`
	// Hold, when set, proposes the command for a human decision instead of
	// applying it: see HoldProposal. It is never part of the command's
	// fingerprint.
	Hold *HoldProposal `json:"hold,omitempty"`
}

// Ref is the generic resource reference used by policy, events and storage
// records. It carries tenant and inbox scope only: for conversation-scoped
// kinds (message, task, work item, stream) the conversation lives on the typed
// ref, so a Ref cannot be turned back into one. Inbox is empty for
// tenant-scoped kinds and required for inbox- and conversation-scoped kinds.
type Ref struct {
	Tenant TenantID     `json:"tenant"`
	Kind   ResourceKind `json:"kind"`
	Inbox  ID           `json:"inbox"`
	ID     ID           `json:"id"`
}
type ChannelRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}
type InboxRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}
type ContactRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}
type IdentityRef struct {
	Inbox InboxRef `json:"inbox"`
	ID    ID       `json:"id"`
}
type ConversationRef struct {
	Inbox InboxRef `json:"inbox"`
	ID    ID       `json:"id"`
}
type MessageRef struct {
	Conversation ConversationRef `json:"conversation"`
	ID           ID              `json:"id"`
}
type OperationRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}
type TaskRef struct {
	Conversation ConversationRef `json:"conversation"`
	ID           ID              `json:"id"`
}
type WorkItemRef struct {
	Conversation ConversationRef `json:"conversation"`
	ID           ID              `json:"id"`
}

// AttachmentRef is inbox-scoped: attachments are staged per inbox, and the
// inbox is their access boundary.
type AttachmentRef struct {
	Inbox InboxRef `json:"inbox"`
	ID    ID       `json:"id"`
}
type SubscriptionRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}

// ExportRef is tenant-scoped: an export may span inboxes.
type ExportRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}

type ExternalRef struct {
	Channel   ChannelRef `json:"channel"`
	Namespace string     `json:"namespace"`
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	ParentID  string     `json:"parent_id"`
}

// Identity is a channel identity within one inbox. Assurance is source
// assurance set at ingress; LinkAssurance is how strongly the identity is shown
// to belong to Contact, set by linking. Kind is the original contact kind
// recorded at claim (see ContactKindPerson). Attributes are normalized lookup
// hints, never identity proof or authorization.
type Identity struct {
	Ref           IdentityRef   `json:"ref"`
	Contact       ContactRef    `json:"contact"`
	SourceID      string        `json:"source_id"`
	Namespace     string        `json:"namespace"`
	Assurance     string        `json:"assurance"`
	Kind          string        `json:"kind"`
	LinkAssurance LinkAssurance `json:"link_assurance"`
	Attributes    []Attribute   `json:"attributes,omitempty"`
	Revision      Revision      `json:"revision"`
}
type IdentityKey struct {
	Inbox     InboxRef `json:"inbox"`
	Namespace string   `json:"namespace"`
	SourceID  string   `json:"source_id"`
}

// Contact is one person, bot or agent. DisplayName and Attributes are set
// by Contacts.Create and Contacts.Update; a contact an identity claim made
// has neither. Attributes are lookup hints, never identity proof or
// authorization. A contact is unified with identities by link only.
type Contact struct {
	Ref         ContactRef  `json:"ref"`
	DisplayName string      `json:"display_name"`
	Kind        string      `json:"kind"`
	Attributes  []Attribute `json:"attributes,omitempty"`
	Revision    Revision    `json:"revision"`
	// MergedInto is the redirect Contacts.Merge leaves: the contact this one
	// was merged into, whose identities it once held. Nil while the contact
	// is live. It is never changed once set, and the pointed-to value is
	// never modified.
	MergedInto *ContactRef `json:"merged_into,omitempty"`
}
type Author struct {
	Identity  IdentityRef `json:"identity"`
	External  ExternalRef `json:"external"`
	Kind      string      `json:"kind"`
	Assurance string      `json:"assurance"`
}

type Visibility string

const (
	Public     Visibility = "public"
	Internal   Visibility = "internal"
	Restricted Visibility = "restricted"
)

type Audience struct {
	Visibility Visibility `json:"visibility"`
	PolicyRef  string     `json:"policy_ref"`
}
type Extension struct {
	Name    string          `json:"name"`
	Version uint32          `json:"version"`
	Data    json.RawMessage `json:"data,omitempty"`
}
type Part struct {
	Kind       string         `json:"kind"`
	Text       string         `json:"text"`
	Attachment *AttachmentRef `json:"attachment,omitempty"`
	Structured *Extension     `json:"structured,omitempty"`
}
type Content struct {
	Parts []Part `json:"parts,omitempty"`
}
type Message struct {
	Ref        MessageRef    `json:"ref"`
	Author     Author        `json:"author"`
	Content    Content       `json:"content"`
	Audience   Audience      `json:"audience"`
	External   []ExternalRef `json:"external,omitempty"`
	Task       *TaskRef      `json:"task,omitempty"`
	OccurredAt time.Time     `json:"occurred_at"`
	RecordedAt time.Time     `json:"recorded_at"`
	Revision   Revision      `json:"revision"`
	// Redacted marks a local privacy redaction (Privacy.Redact): content,
	// attachments and raw ingress scrubbed in uma, no provider call. It
	// never reports a provider deletion; see Deleted.
	Redacted bool `json:"redacted"`
	// Deleted marks a provider deletion: an applied Messages.Delete or an
	// inbound message_deleted envelope. Content is cleared, the record
	// stays as a tombstone, LastEdit is unchanged. A deleted message may
	// also be Redacted.
	Deleted bool    `json:"deleted"`
	Origin  *Origin `json:"origin,omitempty"`
	// ImportedBy is, on a message Runtime.Import recorded from the
	// provider's history, the actor the import ran for
	// (ImportRequest.Requester); it is empty on a message that arrived
	// live through ingress or was authored through uma. An imported
	// message is old news: it never wakes a snooze and never counts as
	// unread (ReadMarker.Unread).
	ImportedBy ActorID     `json:"imported_by"`
	Source     TypeKey     `json:"source"`
	Metadata   []Extension `json:"metadata,omitempty"`
	// EchoOf marks an inbound copy as the provider's echo of a uma-sent
	// message: the copy arrived before the send's answer named its provider
	// message ID, so it was recorded as inbound; once the answer did, it is
	// linked to the outbound message it shows. A copy is kept, never
	// deleted.
	EchoOf *MessageRef `json:"echo_of,omitempty"`
	// Reactions is the message's stored reaction state, as far as uma
	// knows it (see MessageReaction): attributed entries where the
	// provider names who reacted, count-only entries where it gives
	// counts only. Every change bumps Revision.
	Reactions []MessageReaction `json:"reactions,omitempty"`
	// ReactionsComplete reports that Reactions came from an authoritative
	// provider snapshot or refetch; state derived from reaction events,
	// or folded past MaxReactionsPerMessage, is incomplete.
	ReactionsComplete bool `json:"reactions_complete"`
	// LastEdit records who last changed the content; nil when it never
	// changed. Author is untouched by edits.
	LastEdit *Edit `json:"last_edit,omitempty"`
	// InReplyTo is the message this one answers, in the same conversation:
	// on a uma-authored reply, the parent Messages.Reply resolved at
	// commit; on an inbound message, the message its provider reported it
	// replies to (Envelope.InReplyTo), when uma holds that message in the
	// conversation the inbound message joined. It is nil on internal
	// notes, on replies that answer no message, and on inbound messages
	// whose provider reports no reply parent or whose parent uma does not
	// hold there. A thread root is a reply parent only where the provider
	// reports it as one. Stores index it, so the replies to one message
	// are found by their parent, never by recording order.
	InReplyTo *MessageRef `json:"in_reply_to,omitempty"`
	// InResponseTo is the interaction this message answers: set by
	// Interactions.Respond in mode message, and by Messages.Reply as
	// correlation only.
	InResponseTo *InteractionRef `json:"in_response_to,omitempty"`
}
type Origin struct {
	Actor       ActorID      `json:"actor"`
	Operation   OperationRef `json:"operation"`
	Application string       `json:"application"`
}
type Participant struct {
	Identity IdentityRef `json:"identity"`
	Role     string      `json:"role"`
}

// Conversation is a local conversation. Status is one of the Conversation*
// status constants and Mode one of the Mode* selection modes. Owner is the
// identity a per-contact conversation belongs to, or, in per-agent-context
// mode, the identity of the verified principal whose request created the
// conversation: only that principal reaches the context and its tasks
// through an adapter.TaskSink. Owner is nil in other modes. CreatedAt
// orders candidates during selection. Labels are the tenant
// labels the conversation holds, sorted by ID, unique, at most
// MaxConversationLabels (see ValidateLabels); they are part of the
// conversation, so adding or removing one changes its revision. Reading
// the refs needs ActionConversationRead; reading a label's name needs
// ActionLabelRead on the label.
type Conversation struct {
	Ref          ConversationRef `json:"ref"`
	Mode         string          `json:"mode"`
	Status       string          `json:"status"`
	Owner        *IdentityRef    `json:"owner,omitempty"`
	Participants []Participant   `json:"participants,omitempty"`
	External     []ExternalRef   `json:"external,omitempty"`
	ReplyRoute   ExternalRef     `json:"reply_route"`
	CreatedAt    time.Time       `json:"created_at"`
	Revision     Revision        `json:"revision"`
	Labels       []LabelRef      `json:"labels,omitempty"`
	// Assignee is the desk actor the conversation is assigned to
	// (Conversations.Assign); nil when unassigned. Snooze is its snooze
	// (Conversations.Snooze); nil when none is in effect. Both are local
	// workflow state, part of the revision, and zeroed for a reader
	// without ActionConversationReadWorkflow.
	Assignee *ActorID `json:"assignee,omitempty"`
	Snooze   *Snooze  `json:"snooze,omitempty"`
	// Links are the conversation's links (Conversations.Link), sorted by
	// CompareConversationLinks, at most MaxConversationLinks. Workflow
	// state like Assignee; a reader also sees only the links whose far end
	// it may read.
	Links []ConversationLink `json:"links,omitempty"`
	// ProviderState is what each channel instance reported about the
	// conversation at the provider: archived, folder or labels, read,
	// flags, trashed. It is never merged into Status or Labels.
	ProviderState []ProviderConversationState `json:"provider_state,omitempty"`
}
type Selection struct {
	Mode           string `json:"mode"`
	Policy         string `json:"policy"`
	LockToSingle   *bool  `json:"lock_to_single,omitempty"`
	LifecycleRules string `json:"lifecycle_rules"`
}

// Inbox binds one channel instance's destinations to a selection policy.
// Ingress to a Disabled inbox is rejected with code Disabled.
//
// DeliverNotes opts the inbox in to delivering internal notes to the
// provider as private notes (such as private ticket comments); its
// channel's adapter must declare CapabilityInternalNote. Off by default:
// notes stay in uma.
//
// Key is an optional host-set name for the inbox, unique among the
// tenant's inboxes, so a host can find it again without storing its ID
// (Configuration.InboxByKey). Empty means none.
type Inbox struct {
	Ref          InboxRef   `json:"ref"`
	Channel      ChannelRef `json:"channel"`
	Key          string     `json:"key"`
	Name         string     `json:"name"`
	Destinations []string   `json:"destinations,omitempty"`
	Selection    Selection  `json:"selection"`
	Disabled     bool       `json:"disabled"`
	DeliverNotes bool       `json:"deliver_notes"`
	Revision     Revision   `json:"revision"`
}

// Channel is one configured adapter instance. Key is an optional host-set
// name for the channel, unique among the tenant's channels
// (Configuration.ChannelByKey). Empty means none.
type Channel struct {
	Ref           ChannelRef `json:"ref"`
	Key           string     `json:"key"`
	Type          TypeKey    `json:"type"`
	Configuration Extension  `json:"configuration"`
	CredentialRef string     `json:"credential_ref"`
	Revision      Revision   `json:"revision"`
}

// WorkItem is the binding of one provider issue or ticket (External) to the
// conversation of its Ref, with the provider's last applied state. Status
// is the declared mapping of RawStatus (see WorkItemOpen); Binding, its
// Successor and History record merges, moves, deletions and loss of access
// without ever dropping the binding. LocalFields are the fields uma owns;
// every other field follows the provider. Source is the provider version
// of the latest applied state.
type WorkItem struct {
	Ref         WorkItemRef             `json:"ref"`
	External    ExternalRef             `json:"external"`
	Subject     string                  `json:"subject"`
	URL         string                  `json:"url"`
	Status      string                  `json:"status"`
	RawStatus   string                  `json:"raw_status"`
	Requester   ExternalRef             `json:"requester"`
	Assignees   []ExternalRef           `json:"assignees,omitempty"`
	Fields      []Extension             `json:"fields,omitempty"`
	Binding     string                  `json:"binding"`
	Successor   *ExternalRef            `json:"successor,omitempty"`
	History     []WorkItemBindingChange `json:"history,omitempty"`
	LocalFields []string                `json:"local_fields,omitempty"`
	Source      WorkItemSource          `json:"source"`
	Revision    Revision                `json:"revision"`
}

// Task is an agent protocol task in a per-agent-context conversation: work
// the host executes for a remote agent, many per conversation, each with its
// own lifecycle. Context is the conversation's context reference
// (RefKindContext); External is the task's core reference,
// TaskExternalRef(Context.Channel, Ref.ID), which the protocol shows as the
// task ID. State is a stored task state (IsTaskState). StatusMessage is the
// uma-authored message holding the latest status content, in the task's
// conversation; nil when the status has no content. StatusAt is when the
// state or status message last changed, CreatedAt when the task was
// opened. CancelRequested records a remote cancel request the host has not
// yet answered; reaching TaskCanceled clears it. Artifacts are the task's
// outputs, unique by ID. See Validate.
type Task struct {
	Ref             TaskRef     `json:"ref"`
	Context         ExternalRef `json:"context"`
	External        ExternalRef `json:"external"`
	State           string      `json:"state"`
	StatusMessage   *MessageRef `json:"status_message,omitempty"`
	StatusAt        time.Time   `json:"status_at"`
	CancelRequested bool        `json:"cancel_requested"`
	Artifacts       []Artifact  `json:"artifacts,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	Revision        Revision    `json:"revision"`
}

type PageRequest struct {
	Limit int    `json:"limit"`
	After Cursor `json:"after"`
}

// Page is one page of a list. Each item was authorized for the caller; one
// the caller may not read is left out without trace, with no total. A page
// can be short or empty and still carry Next, so loop until Complete. Next
// is the store position the scan reached, which can reveal that records the
// caller may not read exist; only searching message lists and operation
// lists seal the page that completes the query (no Next). The README
// section "Searching messages" states this accepted limit.
type Page[T any] struct {
	Items    []T    `json:"items,omitempty"`
	Next     Cursor `json:"next"`
	Complete bool   `json:"complete"`
}

// Filter narrows a query. Each query honours a documented subset of the
// fields and rejects any other set field with Validation, never ignoring
// one. Identity selects link history (Contacts.History) and, in the
// store's task list (store.Reader.Tasks), the owner of the tasks'
// conversations; every other query rejects it. Labels selects
// conversations, and the messages of conversations, by the labels they
// hold (LabelSelector); subscriptions reject it. Task selects the message history of one task (the messages
// bound to it, Message.Task); a query that does not honour it rejects it.
// Text, Since, Before and Attachments are the message search fields; see
// MaxSearchTextBytes for their semantics. Since and Before also bound the
// time an operation was written for the operation list (Operations.List,
// store.Reader.Operations), which honours Inboxes, Conversation, Statuses
// (operation states), Since and Before. Operations.List also honours Kinds
// (operation kinds) and Actor, which it evaluates itself: they never reach
// the store. The event list (Events.List, store.Reader.Events) honours
// Inboxes, Conversation, Kinds and Since (on Event.RecordedAt);
// subscriptions honour Inboxes and Kinds only.
type Filter struct {
	Inboxes      []InboxRef       `json:"inboxes,omitempty"`
	Conversation *ConversationRef `json:"conversation,omitempty"`
	Contact      *ContactRef      `json:"contact,omitempty"`
	Kinds        []string         `json:"kinds,omitempty"`
	Statuses     []string         `json:"statuses,omitempty"`
	Identity     *IdentityRef     `json:"identity,omitempty"`
	Labels       *LabelSelector   `json:"labels,omitempty"`
	Task         *TaskRef         `json:"task,omitempty"`
	// Text selects messages holding every token of Text (SearchTokens) in
	// their text parts. Empty means unset.
	Text string `json:"text"`
	// Since selects messages with OccurredAt at or after Since; for
	// Operations.List, operations written at or after it; and for
	// Events.List, events with RecordedAt at or after it.
	Since *time.Time `json:"since,omitempty"`
	// Before selects messages with OccurredAt strictly before Before, and,
	// for Operations.List, operations written strictly before it; with Since
	// set it must be later than Since.
	Before *time.Time `json:"before,omitempty"`
	// Attachments selects messages by their attachment parts.
	Attachments *AttachmentFilter `json:"attachments,omitempty"`
	// Assignee selects conversations by assignee; Snoozed, when set,
	// keeps only the conversations whose snooze is in effect (true) or
	// only the others (false): an imported message (Message.ImportedBy)
	// never ends a snooze. Both are workflow filters: a principal
	// without ActionConversationReadWorkflow on a conversation never
	// receives it through them.
	Assignee *AssigneeFilter `json:"assignee,omitempty"`
	Snoozed  *bool           `json:"snoozed,omitempty"`
	// Linked, when set, keeps the conversations holding a link (true) or
	// none (false); Unread keeps those holding a message unread for the
	// calling principal (ReadMarker.Unread: an imported message never
	// is). Both are workflow filters.
	Linked *bool         `json:"linked,omitempty"`
	Unread *UnreadFilter `json:"unread,omitempty"`
	// Actor selects the operations an actor sent: those whose
	// Operation.Key names it as the caller. Only Operations.List honours
	// it; every other query and subscriptions reject it.
	Actor *ActorID `json:"actor,omitempty"`
}
type ListRequest struct {
	Call
	Filter Filter      `json:"filter"`
	Page   PageRequest `json:"page"`
}
type Lookup[T any] struct {
	Call
	Ref T `json:"ref"`
}

// LookupKey reads the one record of the principal's tenant holding a
// host-set key.
type LookupKey struct {
	Call
	Key string `json:"key"`
}

type DeliveryState string

const (
	Queued DeliveryState = "queued"
	// RetryPending: an attempt failed without applying the effect, or was
	// abandoned before dispatch, and the next attempt is scheduled. Failure
	// holds that failure and its retry advice (and Retry-After). It is not
	// final: the operation moves on when the retry runs.
	RetryPending     DeliveryState = "retry_pending"
	ProviderAccepted DeliveryState = "provider_accepted"
	Delivered        DeliveryState = "delivered"
	Failed           DeliveryState = "failed"
	Unknown          DeliveryState = "outcome_unknown"
	Rejected         DeliveryState = "rejected"
	Committed        DeliveryState = "committed"
	// DeadLettered: the operation's work was quarantined after repeated
	// claims that never finished; nothing runs until an administrative
	// replay (Operations.Replay) queues it again.
	DeadLettered DeliveryState = "dead_lettered"
	// Held: a plugin actor's proposal waits for a human decision. It
	// precedes queued, has no work, attempt or provider effect, and
	// Operation.Held carries the proposal. Approval queues it, rejection
	// moves it to rejected, a missed deadline to expired.
	Held DeliveryState = "held"
	// Expired: a held operation's deadline passed with no decision. It is
	// final and applied nothing.
	Expired DeliveryState = "expired"
)

// Operation is the inspectable state of an accepted command. Result is the
// generic reference of the resource the operation produced or, for a
// message mutation (Edit, Delete, React), targets. A generic Ref cannot
// rebuild conversation-scoped refs, so message results (Reply,
// AddInternalNote) and mutation targets also set ResultMessage, stream
// results (StartStream, StreamUpdate, EndStream) set ResultStream and task
// results (Tasks.Act) set ResultTask; when set, they agree with Result. An
// ephemeral reply creates no message.
//
// ReadConfirmedAt is when the provider first reported the message of a
// delivered reply or conversation start read, as the adapter gave it
// (DeliveryReport.ReadAt). The state stays delivered; nil when no read was
// reported. Provider status reports move a settled reply up by rank, never
// down (provider_accepted, failed, delivered, delivered and read), with one
// delivery.updated event per change; see the README's delivery status
// section.
//
// Pending is the unapplied change of an unsettled mutation, as
// Operations.Get reveals it (see PendingMutation); it is never stored.
// PendingState is the same for an unsettled provider state operation (see
// PendingState).
//
// NextAttemptAt is when the runtime next acts on an operation that waits:
// the next reconciliation of an outcome_unknown operation, the next retry
// of a retry_pending one. It is the earliest time a worker may claim the
// scheduled work, not a promise that one runs then, and Operations.Reconcile
// does not wait for it. It is absent when nothing is scheduled: a settled
// operation, one whose latest attempt is under way, a dead-lettered one, and
// an outcome_unknown one the runtime no longer reconciles, such as an
// adapter without reconcile.
//
// UpdatedAt is when the operation was last written: the time the store
// recorded when it accepted the command or last changed it, to the
// microsecond and in UTC. The store sets it on every read, from the same
// write time Operations.List orders by and bounds with Filter.Since and
// Filter.Before, so an operation listed in a window shows a time inside
// it. Any write moves it forward (a state change, a retry schedule, a
// progress step); the revision does not say when. A caller never supplies
// it: a value in a record handed to the store is ignored. It is zero only
// for an operation a caller built itself.
//
// Key is the operation key the command claimed, without its tenant: who
// sent it, its kind, its target and the command's operation ID. Every
// operation uma returns carries it (an Acceptance, Operations.Get, Lookup,
// List and Reconcile), so a listed operation can be found again with
// Operations.Lookup and its kind shown; see OperationKey. Like Pending it
// is derived when the operation is read and never stored: writers leave it
// nil.
type Operation struct {
	Ref                OperationRef       `json:"ref"`
	State              DeliveryState      `json:"state"`
	Result             *Ref               `json:"result,omitempty"`
	ResultMessage      *MessageRef        `json:"result_message,omitempty"`
	ResultStream       *StreamRef         `json:"result_stream,omitempty"`
	ResultTask         *TaskRef           `json:"result_task,omitempty"`
	ProviderReferences []ExternalRef      `json:"provider_references,omitempty"`
	ReadConfirmedAt    *time.Time         `json:"read_confirmed_at,omitempty"`
	Failure            *Error             `json:"failure,omitempty"`
	Progress           *OperationProgress `json:"progress,omitempty"`
	Pending            *PendingMutation   `json:"pending,omitempty"`
	PendingState       *PendingState      `json:"pending_state,omitempty"`
	NextAttemptAt      *time.Time         `json:"next_attempt_at,omitempty"`
	UpdatedAt          time.Time          `json:"updated_at"`
	Revision           Revision           `json:"revision"`
	Key                *OperationKey      `json:"key,omitempty"`
	// Held is the proposal of an operation a plugin actor proposed for a
	// human decision (see HeldBlock); nil for every other operation.
	Held *HeldBlock `json:"held,omitempty"`
}

// OperationProgress reports how far an operation that runs in steps has
// come. For a cascade redaction, Done counts the owned messages redacted
// so far, in recording order: the first Done of them are tombstones.
//
// Partial marks a contact cascade that skipped at least one identity on
// the contact its actor may not redact (see policy.CascadePolicy): the
// skipped identities' messages, attachments and link history are left as
// they are. It is set at submission and never says which identities, how
// many or in which inboxes.
type OperationProgress struct {
	Done    int64 `json:"done"`
	Partial bool  `json:"partial"`
}

// AlreadyHeld is set on the acceptance of a proposal that was not stored
// because an operation of the same kind on the same target is already
// held: Operation is that held operation, under another key. A held start
// is a duplicate only for the same recipient, and a held label create only
// for the same label name (HeldBlock.Scope).
type Acceptance struct {
	Operation   Operation `json:"operation"`
	Durable     bool      `json:"durable"`
	Replayed    bool      `json:"replayed"`
	AlreadyHeld bool      `json:"already_held"`
}

// IngressReceipt answers an accepted ingress. Committed reports that the
// ingress unit committed; Duplicate that the event identity was already
// claimed, in which case the receipt is the one stored for the first
// acceptance (Committed false while that one is still unprocessed).
//
// The references correlate the request with what it produced, so a retry
// recovers them: Conversation is the conversation the message landed in,
// Context its context reference (per-agent-context inboxes), Message the
// recorded message and Task the task it opened or continued. They are
// filled by the ingress unit, persisted with the receipt and returned
// unchanged on every duplicate. Only a committed receipt carries them; see
// Validate.
//
// Operation answers a status report or a marked echo (see EchoRef): it is
// the operation whose attempt the envelope recorded delivery evidence for.
// It is nil when the envelope named no attempt, or named an operation or
// attempt the engine does not hold: such an envelope is accepted and
// settles nothing, and this is how a caller learns it. Like the other
// references it is persisted and returned unchanged on every duplicate.
type IngressReceipt struct {
	ID           ID               `json:"id"`
	Durable      bool             `json:"durable"`
	Committed    bool             `json:"committed"`
	Duplicate    bool             `json:"duplicate"`
	Conversation *ConversationRef `json:"conversation,omitempty"`
	Context      *ExternalRef     `json:"context,omitempty"`
	Message      *MessageRef      `json:"message,omitempty"`
	Task         *TaskRef         `json:"task,omitempty"`
	Operation    *OperationRef    `json:"operation,omitempty"`
}
type CapabilityStatus struct {
	Name      Capability `json:"name"`
	Available bool       `json:"available"`
	Reason    ErrorCode  `json:"reason"`
	Limits    Extension  `json:"limits"`
}
type ValidationIssue struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Event is a committed domain event. Tenant and Inbox repeat Resource scope
// for filtering and must equal it, except that an event about an operation
// may carry the inbox of the operation's target; see Validate.
type Event struct {
	Version     uint32        `json:"version"`
	ID          ID            `json:"id"`
	Kind        string        `json:"kind"`
	Tenant      TenantID      `json:"tenant"`
	Inbox       ID            `json:"inbox"`
	Resource    Ref           `json:"resource"`
	Actor       ActorID       `json:"actor"`
	OccurredAt  time.Time     `json:"occurred_at"`
	RecordedAt  time.Time     `json:"recorded_at"`
	Correlation CorrelationID `json:"correlation"`
	Causation   ID            `json:"causation"`
	Audience    Audience      `json:"audience"`
	Revision    Revision      `json:"revision"`
	Payload     Extension     `json:"payload"`
}
