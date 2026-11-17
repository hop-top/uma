package model

import (
	"encoding/json"
	"time"
)

type ConfigureChannel struct {
	Command
	Channel Channel `json:"channel"`
}
type ConfigureInbox struct {
	Command
	Inbox Inbox `json:"inbox"`
}

// Evidence is identity-link evidence. Verifier is the registered verifier
// key, which is the evidence kind (EvidenceKinds). Reference is an opaque,
// single-use reference to the proof, such as a challenge or import record
// ID: one (Verifier, Reference) pair backs at most one link, and the
// reference outlives redaction, so it must carry no personal data. Facts
// are the kind's versioned facts.
//
// On a request (LinkIdentity, UnlinkIdentity) evidence is the verifier's
// input: Assurance must be empty, because assurance comes from the verifier
// only and no request field can declare or waive it. On a link record it is
// what the verifier returned and core checked (ValidateReturn): Assurance is
// the level the verifier vouches for. Core persists verifier-returned
// evidence, never the caller's.
type Evidence struct {
	Verifier  string        `json:"verifier"`
	Reference string        `json:"reference"`
	Facts     Extension     `json:"facts"`
	Assurance LinkAssurance `json:"assurance"`
}

// LinkIdentity points an identity at an existing contact: a link off its
// provisional contact or a relink between contacts, classified by the
// engine. Contact is required; there is no empty-ID unlink sentinel
// (UnlinkIdentity detaches). Evidence is required. ExpectedRevision is the
// identity's revision.
type LinkIdentity struct {
	Command
	Identity IdentityRef `json:"identity"`
	Contact  ContactRef  `json:"contact"`
	Evidence Evidence    `json:"evidence"`
}

// UnlinkIdentity detaches an identity to a fresh contact the engine
// allocates. Reason is the actor's audit reason code, one lowercase name
// segment, recorded on the link record; it outlives redaction, so it must
// carry no personal data. Evidence is optional: zero means none, and
// supplied evidence is verified for the unlink intent only. It
// establishes authority to detach, never that two identities belong to
// different people. ExpectedRevision is the identity's revision.
type UnlinkIdentity struct {
	Command
	Identity IdentityRef `json:"identity"`
	Reason   string      `json:"reason"`
	Evidence Evidence    `json:"evidence"`
}

// LinkRequest is what an IdentityVerifier verifies: the action-aware request
// core builds from a LinkIdentity or UnlinkIdentity after reading the
// identity. Intent is the class core computed (link, relink or unlink),
// Current the identity's contact and ExpectedRevision its revision. Target
// is the contact to link to, set for link and relink and absent for unlink.
// Reason is the unlink reason code, empty otherwise. Evidence is the
// caller's evidence, the verifier's input.
type LinkRequest struct {
	Intent           LinkClass   `json:"intent"`
	Identity         IdentityRef `json:"identity"`
	Current          ContactRef  `json:"current"`
	ExpectedRevision Revision    `json:"expected_revision"`
	Target           *ContactRef `json:"target,omitempty"`
	Reason           string      `json:"reason"`
	Evidence         Evidence    `json:"evidence"`
}

// Reply is a public reply in a conversation. InReplyTo names the message
// it answers; nil answers the conversation's latest inbound message the
// caller may read, and none when there is no such public message.
// InResponseTo correlates the reply with an interaction of the same
// conversation; it is recorded on the message and in its message.created
// event only, and never answers the interaction (Interactions.Respond
// does): it claims no response, counts no follow-up and reads no token.
// Recipients, when set, addresses the reply beyond the conversation's
// reply route (reply to all, Cc, Bcc); the channel's adapter must declare
// CapabilityReplyRecipients.
type Reply struct {
	Command
	Conversation ConversationRef  `json:"conversation"`
	InReplyTo    *MessageRef      `json:"in_reply_to,omitempty"`
	Content      Content          `json:"content"`
	InResponseTo *InteractionRef  `json:"in_response_to,omitempty"`
	Recipients   *ReplyRecipients `json:"recipients,omitempty"`
}

// StartConversation sends the first message of an outbound-first
// conversation: Content to Recipient through Inbox, in the conversation
// the inbox's selection policy picks or creates for it. Recipient is the
// route the inbox's channel adapter reports for the counterpart: in a
// per-contact inbox a sender reference (an identity namespace the adapter
// declares, its route kind and the canonical ID it reports inbound), in a
// per-room or per-thread inbox the room or thread reference.
type StartConversation struct {
	Command
	Inbox     InboxRef    `json:"inbox"`
	Recipient ExternalRef `json:"recipient"`
	Content   Content     `json:"content"`
}
type InternalNote struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Content      Content         `json:"content"`
	PolicyRef    string          `json:"policy_ref"`
}

// EditMessage replaces the content of a message. Any message the policy
// allows the caller to edit (message.edit) under the message's audience is
// a target, uma-authored or not, when the provider account may edit it;
// the adapter rejects what the account cannot. Content changes only once
// the provider applied the edit (or an inbound copy confirmed it): until
// then the operation shows it as Pending. ExpectedRevision is required.
// The audience never changes; Author is kept and Message.LastEdit records
// the editor.
type EditMessage struct {
	Command
	Message MessageRef `json:"message"`
	Content Content    `json:"content"`
}

// DeleteMessage deletes a message at the provider; once applied, the
// message is a tombstone (Message.Deleted, content cleared). Targets and
// authorization follow EditMessage under message.delete. ExpectedRevision
// is required. It is no privacy redaction: see Privacy.Redact.
type DeleteMessage struct {
	Command
	Message MessageRef `json:"message"`
}

// Reaction adds the caller's reaction Emoji (ValidateEmoji) to a message,
// or removes it when Remove is set. Targets and authorization follow
// EditMessage under message.react. ExpectedRevision is optional and
// compared only when set.
type Reaction struct {
	Command
	Message MessageRef `json:"message"`
	Emoji   string     `json:"emoji"`
	Remove  bool       `json:"remove"`
}
type Transition struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	State        string          `json:"state"`
}
type WorkItemAction struct {
	Command
	Target WorkItemRef `json:"target"`
	Action Extension   `json:"action"`
}

// TaskAction acts on an agent task through Tasks.Act. Action is a builtin
// task action schema (SchemaTaskOpen, SchemaTaskUpdate) read with
// DecodeTaskAction. Target names the task; for SchemaTaskOpen, which
// allocates one, it names only the conversation and its ID is empty.
type TaskAction struct {
	Command
	Target TaskRef   `json:"target"`
	Action Extension `json:"action"`
}
type ExecuteExtension struct {
	Command
	Target   Ref       `json:"target"`
	Audience Audience  `json:"audience"`
	Action   Extension `json:"action"`
}
type QueryExtension struct {
	Call
	Target   Ref       `json:"target"`
	Audience Audience  `json:"audience"`
	Query    Extension `json:"query"`
}

// Redact asks for retained content to be replaced by tombstones. Target
// names a message, interaction or attachment, or, with Cascade, a contact,
// identity or conversation whose messages and their attachments are all
// redacted. A generic Ref cannot rebuild a conversation-scoped ref, so a
// message target also sets Message and an interaction target Interaction,
// which must agree with Target and are absent for every other kind. Cascade is required for the cascading kinds and refused for
// the others: redacting a message never cascades. Reason is an optional
// safe token recorded with the request. PropagateToProvider asks for the
// provider's copy to be removed as well.
type Redact struct {
	Command
	Target              Ref             `json:"target"`
	Message             *MessageRef     `json:"message,omitempty"`
	Interaction         *InteractionRef `json:"interaction,omitempty"`
	Cascade             bool            `json:"cascade"`
	Reason              string          `json:"reason"`
	PropagateToProvider bool            `json:"propagate_to_provider"`
}

// OpenSubscription opens a subscription. Start is StartBeginning,
// StartTail or StartCheckpoint, with no default; After is the checkpoint to
// resume from and is set exactly when Start is StartCheckpoint. PayloadMajor
// is the builtin payload major version the consumer accepts; it is
// required. It does not govern events of registered extension kinds, which
// are delivered in the version emitted: consumers switch on
// Event.Payload.Version.
type OpenSubscription struct {
	Command
	Consumer     string `json:"consumer"`
	Filter       Filter `json:"filter"`
	Start        string `json:"start"`
	After        Cursor `json:"after"`
	PayloadMajor uint32 `json:"payload_major"`
}
type Poll struct {
	Call
	Subscription SubscriptionRef `json:"subscription"`
	Limit        int             `json:"limit"`
	Wait         time.Duration   `json:"wait_ms"`
}
type EventBatch struct {
	Events         []Event   `json:"events,omitempty"`
	AckToken       string    `json:"ack_token"`
	ExpiresAt      time.Time `json:"expires_at"`
	ScannedThrough Cursor    `json:"scanned_through"`
}
type Acknowledge struct {
	Command
	Subscription SubscriptionRef `json:"subscription"`
	Token        string          `json:"token"`
}
type Subscription struct {
	Ref        SubscriptionRef `json:"ref"`
	Checkpoint Cursor          `json:"checkpoint"`
	Revision   Revision        `json:"revision"`
}
type SubscriptionResult struct {
	Subscription Subscription `json:"subscription"`
	Acceptance   Acceptance   `json:"acceptance"`
}
type AttachmentMetadata struct {
	Name      string   `json:"name"`
	MediaType string   `json:"media_type"`
	Size      int64    `json:"size"`
	Audience  Audience `json:"audience"`
	Digest    string   `json:"digest"`
}
type Attachment struct {
	Ref      AttachmentRef      `json:"ref"`
	Metadata AttachmentMetadata `json:"metadata"`
	Revision Revision           `json:"revision"`
}
type PrepareAttachment struct {
	Command
	Inbox    InboxRef           `json:"inbox"`
	Metadata AttachmentMetadata `json:"metadata"`
}

// ExportRequest asks for an export of the records Filter selects, in
// Format. The filter names its subject: a conversation, a contact, or one
// or more inboxes.
type ExportRequest struct {
	Command
	Filter Filter `json:"filter"`
	Format string `json:"format"`
}

// ExportFormatJSONL is the export format of one JSON ExportLine per line.
const ExportFormatJSONL = "jsonl"

// Export is a stored export artifact: the filter and format it was made
// with, its byte length and sha256 digest, and when it was created. Its
// bytes are read through Privacy.Open, never through a path or URL.
type Export struct {
	Ref       ExportRef `json:"ref"`
	Filter    Filter    `json:"filter"`
	Format    string    `json:"format"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	Revision  Revision  `json:"revision"`
}

// ExportLine is one line of a JSONL export: Type is one of the ExportLine*
// values and Data the record, encoded as the matching model type.
type ExportLine struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ExportLine types. The first line is ExportLineExport, holding the
// Export without its size and digest; the record lines follow. A label
// line holds a Label that an exported conversation references.
const (
	ExportLineExport       = "export"
	ExportLineContact      = "contact"
	ExportLineIdentity     = "identity"
	ExportLineConversation = "conversation"
	ExportLineMessage      = "message"
	ExportLineAttachment   = "attachment"
	ExportLineLabel        = "label"
	// ExportLineInteraction holds an interaction record, never its
	// response token.
	ExportLineInteraction = "interaction"
)

type StreamRef struct {
	Conversation ConversationRef `json:"conversation"`
	ID           ID              `json:"id"`
}

// Stream is one logical streaming response. Mode is a Stream* mode and State
// a Stream* state. NextSequence is the sequence the next update must carry;
// AppliedSequence is the highest update sequence the provider applied.
// LastOperation is the latest accepted start, update or end: each runs only
// after its predecessor's provider attempt has an answer. Deadline is when
// the stream stops waiting on a predecessor whose provider outcome is
// unknown (StartStream.Deadline after the start was accepted).
type Stream struct {
	Ref                StreamRef     `json:"ref"`
	Mode               string        `json:"mode"`
	State              string        `json:"state"`
	Audience           Audience      `json:"audience"`
	NextSequence       uint64        `json:"next_sequence"`
	AppliedSequence    uint64        `json:"applied_sequence"`
	LastOperation      *OperationRef `json:"last_operation,omitempty"`
	ProviderStreamRef  string        `json:"provider_stream_ref"`
	ProviderReferences []ExternalRef `json:"provider_references,omitempty"`
	Deadline           time.Time     `json:"deadline"`
	Revision           Revision      `json:"revision"`
}

// StartStream opens a stream. Deadline bounds how long, from the start,
// the stream's commands wait behind a predecessor whose provider outcome
// is unknown while the adapter reconciles it; when it passes with a
// command still waiting, the stream ends partial. Zero means
// DefaultStreamDeadline; it is at most MaxStreamDeadline.
type StartStream struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Audience     Audience        `json:"audience"`
	Mode         string          `json:"mode"`
	Deadline     time.Duration   `json:"deadline_ms"`
}
type StreamUpdate struct {
	Command
	Stream   StreamRef `json:"stream"`
	Sequence uint64    `json:"sequence"`
	Content  Content   `json:"content"`
}
type EndStream struct {
	Command
	Stream        StreamRef `json:"stream"`
	FinalSequence uint64    `json:"final_sequence"`
	Cancel        bool      `json:"cancel"`
}
type Signal struct {
	Call
	Conversation ConversationRef `json:"conversation"`
	Kind         string          `json:"kind"`
	Audience     Audience        `json:"audience"`
	ExpiresAt    time.Time       `json:"expires_at"`
}

// Ephemeral sends a recipient-bound ephemeral reply. Interaction is
// optional: nil sends it independently of any interaction; set, it names an
// interaction of the same conversation, answered as a follow-up response.
type Ephemeral struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Recipient    IdentityRef     `json:"recipient"`
	Content      Content         `json:"content"`
	ExpiresAt    time.Time       `json:"expires_at"`
	Interaction  *InteractionRef `json:"interaction,omitempty"`
}

// Envelope is adapter output. Its fields alone never establish verified account/tenant authority.
type Envelope struct {
	EventID   string `json:"event_id"`
	MessageID string `json:"message_id"`
	// InReplyTo is the provider message ID, in the form MessageID takes on
	// this channel, of the message this new message replies to, from
	// provider evidence the adapter verified before Accept (a quoted or
	// replied-to message; an email In-Reply-To). Only an explicit reply
	// to one message counts: thread membership, a forward and a quote of
	// no message do not. Ingress resolves it to Message.InReplyTo when
	// the inbox holds that message in the conversation the new message
	// joins, and otherwise records no link. Only a new message carries it.
	InReplyTo      string            `json:"in_reply_to"`
	Destination    string            `json:"destination"`
	Kind           string            `json:"kind"`
	Sender         ExternalRef       `json:"sender"`
	Assurance      string            `json:"assurance"`
	SystemActor    bool              `json:"system_actor"`
	References     []ExternalRef     `json:"references,omitempty"`
	Content        Content           `json:"content"`
	Audience       Audience          `json:"audience"`
	OccurredAt     time.Time         `json:"occurred_at"`
	SourceRevision string            `json:"source_revision"`
	Payload        Extension         `json:"payload"`
	Interaction    *InteractionInput `json:"interaction,omitempty"`
	// WorkItem is the provider's view of the issue or ticket the envelope's
	// work_item reference names, from a ticketing adapter. It is accepted
	// only by per-work-item inboxes.
	WorkItem *WorkItemState `json:"work_item,omitempty"`
	// Delivery is the delivery evidence of a status envelope for the
	// outbound attempt its EchoRef names. Untagged evidence names no
	// attempt and settles nothing.
	Delivery *DeliveryReport `json:"delivery,omitempty"`
	// Task binds a message envelope to an agent task, accepted only by
	// per-agent-context inboxes: a continuation names an existing task
	// (the context reference may then be omitted: the engine resolves the
	// context through the task), Open asks the engine to open one for
	// the message. Nil binds no task: a message-only exchange.
	Task *TaskBinding `json:"task,omitempty"`
	// Event is the registered event of an extension.event envelope (see
	// EnvelopeExtensionEvent): Name a registered event-kind definition,
	// Version one of its registered versions, Data its payload of
	// references and change summary only. Every other kind carries none.
	Event *Extension `json:"event,omitempty"`
	// SenderKind is the contact kind of Sender (ContactKindPerson,
	// ContactKindBot, ContactKindAgent), from provider evidence the adapter
	// verified before Accept, as Sender itself. Empty is a person. Ingress
	// records it on the sender identity and its provisional contact when it
	// first claims them; an identity already claimed keeps its stored kind.
	// It is a classification, never authentication, link evidence or a
	// policy grant, and independent of EnvelopeAutomatedMessage. Only kinds
	// that claim a sender carry it: new messages and message updates; a
	// kind other than person must be declared in the adapter descriptor's
	// SenderKinds.
	SenderKind string `json:"sender_kind"`
	// SenderAttributes are the attributes of Sender the adapter verified
	// before Accept, as Sender itself: a phone number, a WhatsApp
	// business-scoped user ID. Ingress records them on the sender identity
	// when it first claims it, sorted by kind then value; an identity
	// already claimed keeps its stored attributes. They are lookup hints and
	// verified-attribute link evidence for a host's verifier, never part of
	// the identity key, authorization or a policy grant, and a provisional
	// contact gets none. At most MaxContactAttributes, each valid and
	// normalized (Attribute.Validate), no kind and value twice. Only kinds
	// that claim a sender carry them, as SenderKind.
	SenderAttributes []Attribute `json:"sender_attributes,omitempty"`
	// State is the provider's conversation state of a conversation.state
	// envelope (see EnvelopeConversationState). Every other kind carries
	// none.
	State *ProviderStateReport `json:"state,omitempty"`
}

// DeliveryReport is provider evidence of what became of an outbound
// attempt: provider_accepted, delivered, failed or rejected, and for a
// failure its safe summary. A failure reported after the attempt reached
// the provider is final: the attempt is never repeated.
//
// Reports are weighed, not applied in arrival order: provider_accepted
// ranks below failed, failed below delivered, delivered below delivered
// with a read time, so a late or repeated report never lowers what an earlier
// one showed.
type DeliveryReport struct {
	State   DeliveryState    `json:"state"`
	Failure *DeliveryFailure `json:"failure,omitempty"`
	// ReadAt is when the recipient read the message, from the provider. It
	// qualifies a delivered report: the engine records the earliest
	// confirmation it holds as Operation.ReadConfirmedAt and the operation
	// stays delivered. A zero time, or one on any other state, is refused.
	// Nil reports no read confirmation, which is every report made before
	// this field existed.
	ReadAt *time.Time `json:"read_at,omitempty"`
}
type Ingress struct {
	Call
	Envelope       Envelope `json:"envelope"`
	ReceiverCursor string   `json:"receiver_cursor"`
}
type InteractionDecision struct {
	FieldErrors map[string]string `json:"field_errors,omitempty"`
	Options     Extension         `json:"options"`
}
