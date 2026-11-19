// Package adapter defines what a provider implements to join an engine: a
// Factory that describes and opens channel instances, the Sink an instance
// reports ingress through, and the optional capability interfaces an
// instance implements for exactly the capabilities its Descriptor declares.
package adapter

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// ContractMajor is the adapter contract major this package defines. An
// engine registers only descriptors whose ContractMajor equals it, and
// accepts the current major only. The contract is frozen: a breaking change
// to these interfaces, or to behaviour an adapter relies on, increments the
// major in one commit with a migration note here, in a minor release of
// v0.x and never in a patch. Additive changes (new optional interfaces or
// fields, new capability names) do not. The adapter conformance suite is the
// test that an adapter meets the major it declares. See
// https://github.com/hop-top/uma/blob/next/docs/compatibility.md.
//
// Major 3 replaced the interaction input (model.InteractionInput) on
// Sink.EvaluateInteraction and model.Envelope.
const ContractMajor uint32 = 3

// Descriptor is what an adapter declares about itself at registration:
// type key, contract major, configuration schema, capabilities, events,
// identity namespaces, sender kinds and exchange semantics. The engine
// validates it once, at New.
type Descriptor struct {
	Type                model.TypeKey
	ContractMajor       uint32
	ConfigurationSchema model.Extension
	Capabilities        []model.Capability
	Events              []string
	IdentityNamespaces  []string
	RequiredPermissions []string
	Maturity            string
	// ReconcileSettle is how long after an attempt the adapter's
	// reconciler may still miss an applied effect, such as a provider
	// search that indexes sent messages with a lag. Inside it, a
	// reconciliation reporting the effect not found (model.Failed) is
	// inconclusive: the operation stays outcome_unknown and is reconciled
	// again once the window has passed, never sent again. Zero trusts
	// "not found" at once; negative is a composition error.
	ReconcileSettle time.Duration
	// Exchange declares the provider semantics core applies to the
	// adapter's messages. Core keys threading, echo matching and message
	// identity on it, never on the type key.
	Exchange Exchange
	// WorkItemActs declares the work item status transitions the adapter
	// performs at the provider (model.CapabilityWorkItemAct). It is
	// non-empty exactly when that capability is declared; see
	// WorkItemTransition.
	WorkItemActs []WorkItemTransition
	// SenderKinds are the contact kinds other than person the adapter
	// reports on model.Envelope.SenderKind: model.ContactKindBot,
	// model.ContactKindAgent. Empty: every sender is a person. Each entry
	// is a contact kind other than person, at most once; anything else
	// fails registration. Ingress rejects an envelope reporting a kind
	// not declared here.
	SenderKinds []string
}

// Exchange is the declared exchange semantics of an adapter. The zero
// value grants the least trust: message IDs treated as sender-chosen, no
// threading policy. A value other than the declared constants fails
// registration.
type Exchange struct {
	// MessageIDs is who chooses the provider message IDs the adapter
	// reports: model.Envelope.MessageID inbound and model.ProviderMessageRef
	// in an Outcome. Empty is treated as MessageIDsSender.
	MessageIDs MessageIDAuthority
	// Threading is the threading policy of per-contact inboxes on the
	// adapter's channels. Empty applies none.
	Threading Threading
	// Reactions is how many reactions one reactor may hold on a provider
	// message. Empty is ReactionsMultiple.
	Reactions ReactionCardinality
}

// MessageIDAuthority is who assigns a provider message ID.
type MessageIDAuthority string

const (
	// MessageIDsProvider: the provider assigns every message ID, unique per
	// channel, and no sender can choose one. Core then treats an ID as
	// the message's identity: a second report of it in an inbox is the
	// same message, never a new one, and an inbound ID held by a message
	// uma sent is that message's echo.
	MessageIDsProvider MessageIDAuthority = "provider"
	// MessageIDsSender: whoever sends the message chooses its ID, as with
	// an RFC 5322 Message-ID. Core gives such an ID no identity or echo
	// meaning; it may still thread.
	MessageIDsSender MessageIDAuthority = "sender"
)

// Threading is a threading policy.
type Threading string

// ThreadingEmail threads per-contact inboxes on RFC 5322 header
// references: the model.EmailHeaderNamespace references an adapter
// reports from model.EmailHeaders.Normalize. Header references are
// sender-controlled whatever the adapter declares: they select a
// conversation of the same inbox, never establish identity or an echo.
const ThreadingEmail Threading = "email"

// ReactionCardinality is how many reactions one reactor may hold on a
// provider message.
type ReactionCardinality string

const (
	// ReactionsMultiple: a reactor may hold several reactions, one per
	// emoji. The zero value means the same.
	ReactionsMultiple ReactionCardinality = ""
	// ReactionsSingle: a reactor holds at most one reaction, as on
	// WhatsApp. Core applies a reactor's new reaction, outbound or inbound,
	// as replacing the reactor's earlier one, and a removal as removing it
	// whatever its emoji; an inbound removal naming no emoji is then never
	// ambiguous.
	ReactionsSingle ReactionCardinality = "single"
)

type Dependencies struct {
	Credentials ports.Credentials
	HTTP        ports.HTTPClient
	Clock       ports.Clock
	Observer    ports.Observer
	Protocol    ProtocolState
}
type Factory interface {
	Describe() Descriptor
	// Validate checks a channel configuration without I/O. It owns the
	// configuration's fields; core has already checked the schema name and
	// version against Descriptor.ConfigurationSchema. It runs before the
	// channel is stored (Configuration.ConfigureChannel), so a configuration
	// it refuses is never durable.
	Validate(model.Extension) []model.ValidationIssue
	Open(context.Context, model.Channel, Dependencies) (Instance, error)
}

// ActionValidator is an optional Factory interface for an adapter that
// owns the fields of an extension effect (see ExtensionExecutor).
// Extensions.Execute calls ValidateAction after the core checks of the
// action and before the effect is stored, so fields the adapter refuses are
// a model.Validation error at the call with nothing stored, the operation
// ID still free.
//
// ValidateAction does no I/O and keeps no state: it judges the action and
// the audience it is sent under, nothing about the channel or the provider.
// The action's name and version are a registered effect definition by then
// and select the rules. It returns the issues found, none for an action it
// accepts; core refuses on the first, naming its field under "action." and
// its message. The issue's code is model.Validation unless it is
// model.Unsupported or model.SchemaMismatch, which pass through.
//
// Implementing it does not relax ExtensionExecutor: Execute still validates
// the stored action at the attempt, the guard for an action stored before a
// rule tightened, and should share its rules with ValidateAction. Core does
// not repeat ValidateAction at the attempt. Implementing it is not a
// contract change: a Factory without it keeps the late path, and the
// contract major stays as it is.
type ActionValidator interface {
	ValidateAction(audience model.Audience, action model.Extension) []model.ValidationIssue
}

// RecipientNormalizer is an optional Factory interface for an adapter whose
// identifiers have more than one spelling for one provider identity, such as
// an email address that ignores case. Conversations.Start calls
// NormalizeRecipient on the request's recipient once the inbox, its
// channel and the adapter's start_conversation capability are known, before
// the start is checked, fingerprinted or stored. Core then keys on the
// result alone: the recipient's identity in a per-contact inbox, the
// conversation's route and the start's idempotency fingerprint. A start to
// "A@x.org" and one to "a@x.org" reach one identity and one
// conversation, and the same operation key with either spelling is one
// operation, never a conflict.
//
// Return the spelling the adapter's inbound reports for the identity, so a
// reply from the recipient lands on the identity the start created. The
// adapter keeps normalizing its own inbound traffic; core never does.
//
// NormalizeRecipient does no I/O and keeps no state: it is a pure function
// of the recipient. It is idempotent, and it may change recipient.ID only:
// core refuses a result that differs in Channel, Namespace, Kind or
// ParentID, or that is not a valid reference, as a model.Permanent adapter
// fault. Only recipients of the inbox's channel reach it. A start that
// names no identity the adapter can use is refused by returning an error:
// core answers it as a model.Validation error at the call, before anything
// is stored, so the operation ID stays free. A *model.Error's safe message
// is kept; any other error becomes a fixed message, so return a safe one.
//
// It is not called for a send, a reply or any other command: they route on
// what the conversation stored. A conversation or identity stored before an
// adapter began normalizing keeps its stored spelling.
//
// Implementing it is optional and not a contract change: a Factory without
// it keeps working as before and the contract major stays as it is.
type RecipientNormalizer interface {
	NormalizeRecipient(recipient model.ExternalRef) (model.ExternalRef, error)
}

// LegacyRecipientReuser is an optional Factory interface for an adapter that
// implements RecipientNormalizer and whose identities were claimed, before it
// began normalizing, on the recipient exactly as the caller typed it.
// Conversations.Start calls ReuseLegacyRecipient with the recipient as typed
// only in a per-contact inbox and only when NormalizeRecipient changed its ID.
// Reporting true lets the start look for an identity stored under that
// typed spelling: one exact read, made before the start's claiming unit, and
// only when the normalized spelling has no identity in the inbox. A legacy
// identity found there is reused, with its conversations, instead of a second
// identity being claimed. The conversation a start reuses keeps its stored
// route; one the start creates for that identity routes to the normalized
// spelling. The fingerprint stays keyed on the normalized recipient either
// way.
//
// The lookup is exact and deliberately partial. It finds a legacy identity
// only when the caller types the address as they did before; a start typed
// in any other case claims the normalized identity as without the hook.
// When both spellings have an identity the normalized one wins, with no
// error. Inbound is unchanged: the adapter keeps reporting the normalized
// spelling, so a reply claims the normalized identity. Reporting true is a
// statement that a conversation routed to the typed spelling can still be
// sent to. Contacts.PreviewMerge and Contacts.Merge remain the way to join
// identities that both exist.
//
// ReuseLegacyRecipient does no I/O and keeps no state: it is a pure function
// of the recipient. An adapter that does not implement it, or reports false,
// keeps the normalized-only behaviour and costs no read.
//
// Implementing it is optional and not a contract change: a Factory without
// it keeps working as before and the contract major stays as it is.
type LegacyRecipientReuser interface {
	ReuseLegacyRecipient(typed model.ExternalRef) bool
}

// Instance is one opened channel connection. Capabilities reports the
// current status of every declared capability, each exactly once: available
// with no reason, or unavailable with the reason code. The engine fails
// closed: a declared capability the report omits is unavailable, and a
// report naming a capability twice or marking one available with a reason
// is invalid output (model.Permanent) that fails every capability check.
type Instance interface {
	Capabilities(context.Context) ([]model.CapabilityStatus, error)
	Close(context.Context) error
}

// Sink is bound by the engine to a registered instance and service principal.
// Adapters must verify transport/payloads before Accept; no payload flag creates trust.
// The same holds for Envelope.SenderKind: an adapter sets it only from
// provider data it verified, such as a signed webhook payload or a
// host-authenticated principal.
//
// A status envelope (model.EnvelopeStatus) carries a model.DeliveryReport
// for the attempt its model.EchoRef names. The engine weighs it with the
// evidence it holds and moves the operation up, never down, so an adapter
// reports each provider update as it arrives and keeps no order of its own.
// The receipt's Operation names the operation the report reached, and is
// nil when it reached none. See the implementers' guide, "Delivery status
// reports".
//
// EvaluateInteraction asks the host's interaction validator, synchronously
// and read-only, for field errors or dynamic options of one interaction
// (model.InteractionOptions included); it never accepts the interaction.
type Sink interface {
	Accept(context.Context, model.Ingress) (model.IngressReceipt, error)
	EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error)
}

// AttachmentSink is implemented by the engine's sink. An adapter stages the
// bytes of inbound media or a visitor upload before Accept, then names the
// returned attachment in an attachment part of the envelope's content.
//
// StageAttachment reserves the attachment in the inbox the request's
// destination resolves to and streams body into the host's blob store as
// the sink's service principal, under the checks of an Attachments upload:
// the size limit and accepted media types, a declared size and digest
// verified, the audience authorized. Staging is idempotent per channel and
// Key: restaging the same key with the same metadata and bytes returns the
// same attachment; other bytes conflict. Without a blob store it is
// model.Disabled.
//
// Accept takes an attachment part only for a staged attachment of the
// envelope's inbox whose audience equals the envelope's.
type AttachmentSink interface {
	StageAttachment(ctx context.Context, req StageRequest, body io.Reader) (model.AttachmentRef, error)
}

// StageRequest describes inbound bytes to stage.
type StageRequest struct {
	// Key identifies the bytes at the provider, such as a media ID, stable
	// across redeliveries and unique per channel. Required; at most 255
	// bytes.
	Key string
	// Destination is the destination of the ingress the attachment will
	// belong to; the attachment is reserved in its inbox.
	Destination string
	// Metadata is the attachment's name, media type, size (zero when
	// unknown), digest (empty when unknown) and audience.
	Metadata model.AttachmentMetadata
}

// CursorSink is implemented by the engine's sink. A receiver resumes from
// ReceiverCursor after a restart instead of re-reading what the engine
// already holds: it returns the model.Ingress.ReceiverCursor of the latest
// accepted ingress of the channel that carried one, committed in the same
// unit as that ingress (a duplicate included, a rejected one never), or ""
// before any. A cursor is at most 1024 bytes of UTF-8.
type CursorSink interface {
	ReceiverCursor(context.Context) (string, error)
}

// TaskSink is implemented by the engine's sink for an agent protocol
// adapter serving per-agent-context inboxes. It is how such an adapter
// answers protocol reads and cancel requests; every other write goes
// through Accept and StageAttachment. It has no method that opens or
// updates a task: tasks are opened by the host (Tasks.Act) or by an
// opening model.TaskBinding on an accepted message, and only the host
// reports their progress.
//
// Each method runs as the sink's service principal and is authorized as
// such: reads ask model.ActionTaskRead, model.ActionConversationRead and
// model.ActionMessageRead, CancelTask asks model.ActionTaskAct. A denial of
// the one resource a call is about is model.Unauthorized, and nothing is
// read or written; in a page (Tasks, TaskMessages) an item the service
// principal may not read is skipped without trace.
//
// principal is the verified remote principal of the protocol request, in
// the form the adapter reports it as model.Envelope.Sender: the identity
// its host authentication established, never a request field. A context
// and its tasks are reachable only by the principal that owns them: the
// identity, in the conversation's inbox, of the principal whose request
// created the conversation (model.Conversation.Owner). A context or task
// that is absent and one that the principal does not own are the same
// model.NotFound, so a caller learns nothing about resources it cannot
// reach.
type TaskSink interface {
	// Context returns the conversation of the context reference ref
	// (model.RefKindContext).
	Context(ctx context.Context, principal, ref model.ExternalRef) (model.Conversation, error)
	// Task returns the task whose core reference is ref
	// (model.TaskExternalRef).
	Task(ctx context.Context, principal, ref model.ExternalRef) (model.Task, error)
	// Tasks pages the principal's tasks, most recent status first (then
	// task ID, descending), narrowed by query. A context that is absent or
	// not the principal's lists nothing. A principal owning conversations
	// in several per-agent-context inboxes of the channel can list only
	// with query.Context set; without it the call is model.Unsupported.
	Tasks(ctx context.Context, principal model.ExternalRef, query TaskQuery) (model.Page[model.Task], error)
	// TaskMessages pages the message history of a task (the messages
	// bound to it) in recording order.
	TaskMessages(ctx context.Context, principal model.ExternalRef, task model.TaskRef, page model.PageRequest) (model.Page[model.Message], error)
	// Responses reports what answers the accepted message: see Responses.
	Responses(ctx context.Context, principal model.ExternalRef, message model.MessageRef) (Responses, error)
	// CancelTask records a remote cancel request on a task
	// (model.Task.CancelRequested) as the operation of cmd, emitting
	// task.updated, and returns the task in its current state: the host
	// decides whether and when it reaches model.TaskCanceled. A repeated
	// request is a no-op returning the task; a request on a terminal task
	// is model.Conflict.
	CancelTask(ctx context.Context, principal model.ExternalRef, task model.TaskRef, cmd model.Command) (model.Task, error)
}

// TaskQuery narrows TaskSink.Tasks. Context, when set, keeps the tasks of
// that context; States, when set, the tasks in one of those states. Page
// pages the result; a zero Limit takes the engine's default page size.
type TaskQuery struct {
	Context *model.ExternalRef
	States  []string
	Page    model.PageRequest
}

// Validate checks a context reference of kind context, declared task
// states and a non-negative page limit.
func (q TaskQuery) Validate() error {
	invalid := func(field, reason string) error {
		return &model.Error{Code: model.Validation, SafeMessage: field + ": " + reason, Retry: model.NoRetry}
	}
	if q.Context != nil {
		if err := q.Context.Validate(); err != nil {
			// Model validation messages are "field: reason"; name the
			// query field.
			if e, ok := err.(*model.Error); ok {
				prefixed := *e
				prefixed.SafeMessage = "context." + e.SafeMessage
				return &prefixed
			}
			return err
		}
		if q.Context.Kind != model.RefKindContext {
			return invalid("context.kind", "must be context")
		}
	}
	for i, s := range q.States {
		if !model.IsTaskState(s) {
			return invalid("states["+strconv.Itoa(i)+"]", "must be a declared task state")
		}
	}
	if q.Page.Limit < 0 {
		return invalid("page.limit", "must not be negative")
	}
	return nil
}

// Responses is what answers an accepted message, as far as uma recorded
// it. Task is the task the message is bound to (model.Message.Task), and
// is set only once a state change was recorded after the message or the
// message opened it, so a continuation never answers with the task's
// earlier state; nil otherwise. Replies are the uma-authored replies whose
// parent is the message (model.Message.InReplyTo), in recording order,
// never replies to other messages of the same context.
type Responses struct {
	Task    *model.Task
	Replies []model.Message
}

type HTTPReceiver interface{ Handler(Sink) http.Handler }

// UploadReceiver is optionally implemented by an HTTPReceiver whose
// handler takes attachment uploads, staged through AttachmentSink, such as
// a web widget's visitor uploads. Upload reports whether a request is one;
// the engine then bounds its body by the attachment size limit instead of
// the webhook body limit. It must decide from the method and path alone,
// before anything is read.
type UploadReceiver interface{ Upload(*http.Request) bool }
type Receiver interface {
	Run(context.Context, Sink) error
}

// ReceiverReporter is optionally implemented by an Instance whose Receiver
// can tell whether it is connected to the provider, so a host reading
// uma.ReceiverStatus can tell a gateway that is up from one that is only
// being retried inside Run. It is found by type assertion; an instance
// without it is reported with no link state.
//
// ReportReceiver hands the instance report, once per instance, on the
// supervisor's goroutine and before Run starts; it is never called again
// and report is never nil. The receiver calls report(true) when it is
// connected, such as a gateway session established or a poll answered, and
// report(false) when that link is lost or being re-established, from any
// goroutine and as often as it likes: report is safe for concurrent use,
// returns at once, runs no adapter code, does no I/O and cannot panic. Until
// the first call the link reads as not connected. A call made after Run
// returned has no effect. A ReportReceiver that panics is treated as
// absent.
//
// The signal is advisory. It changes no retry, backoff, attempt count or
// lease decision, and a receiver that never calls report, or calls it
// wrongly, behaves as one without the interface; what ends or restarts a
// connection is still what Run returns.
type ReceiverReporter interface {
	ReportReceiver(report func(connected bool))
}

// ReceiverDegradationReporter is optionally implemented by an Instance
// whose Receiver keeps working while part of what it relies on is not, such
// as a mail receiver that still polls after its push registration failed.
// A host reading uma.ReceiverStatus then sees the link as degraded, with a
// stable code, instead of a receiver that looks healthy. It is found by type
// assertion and is independent of ReceiverReporter.
//
// ReportReceiverDegraded hands the instance report, once per instance, on
// the supervisor's goroutine and before Run starts; report is never nil.
// The receiver calls report(code) when it becomes degraded and report("")
// when it recovers, from any goroutine and as often as it likes: report is
// safe for concurrent use, returns at once, runs no adapter code, does no
// I/O and cannot panic. code is a short stable token of the adapter's
// (such as "google.mail.push_denied"), never a provider message or
// anything secret; the adapter documents its codes. A call made after Run
// returned has no effect. A ReportReceiverDegraded that panics is treated
// as absent.
//
// The signal is advisory: it changes no retry, backoff, attempt count or
// lease decision, and what ends or restarts a connection is still what Run
// returns.
type ReceiverDegradationReporter interface {
	ReportReceiverDegraded(report func(code string))
}
type PermissionProber interface {
	Probe(context.Context) ([]model.CapabilityStatus, error)
}

// ProtocolState is per-channel adapter state the engine keeps in its store:
// protocol metadata and cursors, such as a messaging window or a sync
// token, never domain records or secrets. Every instance the engine opens
// receives its channel's state in Dependencies.Protocol; another channel's
// keys are out of reach.
//
// Load returns the value and revision stored under a key, model.NotFound
// when none is. CompareAndSwap stores a value under a key when its current
// revision equals expected, an empty expected meaning the key must not
// exist yet, and fails with model.Conflict otherwise; the engine allocates
// the new revision. Keys are non-empty UTF-8 of at most 256 bytes; keys
// starting with "uma." are reserved for the engine and model.Unauthorized.
type ProtocolState interface {
	Load(context.Context, string) (model.Extension, model.Revision, error)
	CompareAndSwap(context.Context, string, model.Revision, model.Extension) error
}

// Effect is what one provider attempt acts on: the operation and attempt,
// the route and optional target, and the audience. Interaction is set only
// for an interaction response (InteractionResponder) and an ephemeral
// reply naming an interaction; nil otherwise.
type Effect struct {
	Operation   model.OperationRef
	Attempt     model.ID
	Route       model.ExternalRef
	Target      *model.ExternalRef
	Audience    model.Audience
	Interaction *InteractionContext
}

// SendRequest sends new content on the effect's route. A reply naming the
// message it answers carries that message as Effect.Target, its provider
// message on the route's channel, and as Thread.
type SendRequest struct {
	Effect
	Content model.Content
	// Thread is the public message the send answers, nil when it answers
	// none.
	Thread *Thread
	// Recipients addresses the send beyond its route: reply to all and
	// explicit Cc and Bcc, already validated and normalized by core. Set
	// only on a channel declaring model.CapabilityReplyRecipients; nil
	// otherwise. An adapter refuses what its status limits
	// (model.RecipientLimits) do not support with a final rejection.
	Recipients *model.ReplyRecipients
}

// Thread is the message a send answers, as stored: what a provider needs
// to thread the answer, such as email Message-ID and References headers or
// a subject carried in the provider payload.
type Thread struct {
	// References are the message's external references on the route's
	// channel: its provider message IDs and threading headers.
	References []model.ExternalRef
	// Metadata is the message's metadata, the provider payload it arrived
	// with included.
	Metadata []model.Extension
}

// NoteRequest delivers an internal note to the provider as a private
// note, such as a private ticket comment, on the effect's route. Its
// audience is never public; Content is the note as recorded in uma. Only
// an inbox that opted in (model.Inbox.DeliverNotes) sends one, and only to
// an adapter declaring model.CapabilityInternalNote.
type NoteRequest struct {
	Effect
	Content model.Content
}

// NoteSender delivers internal notes (model.CapabilityInternalNote). It
// must never make a note visible to the provider's public audience: a
// request it cannot keep private is rejected.
type NoteSender interface {
	SendNote(context.Context, NoteRequest) (Outcome, error)
}

// EditRequest replaces the content of the provider message
// Effect.Target names, always set: the target message's provider message
// on the route's channel. Content is the whole replacement. The audience
// never changes.
type EditRequest struct {
	Effect
	Content model.Content
}

// DeleteRequest deletes the provider message Effect.Target names, always
// set as for EditRequest.
type DeleteRequest struct{ Effect }

// ReactRequest adds the account's reaction Emoji (model.ValidateEmoji,
// byte-exact, never normalized) to the provider message Effect.Target
// names, always set as for EditRequest, or removes it when Remove is set.
type ReactRequest struct {
	Effect
	Emoji  string
	Remove bool
}
type ExecuteRequest struct {
	Effect
	Action model.Extension
}

// ReferenceChange is one provider message whose reference changed: Old is
// the reference the request named, New the one the provider holds it under
// now. Both are provider message references of the effect's channel
// (model.ProviderMessageRef).
type ReferenceChange struct {
	Old model.ExternalRef
	New model.ExternalRef
}

type Outcome struct {
	State model.DeliveryState
	// References are the provider references of what the effect
	// produced. For a reaction they may name the provider's reaction
	// itself, such as the message that carried it; the engine keeps those
	// on the reaction, never on the target message.
	References []model.ExternalRef
	// ReferenceChanges are, for an applied provider state effect, the
	// provider message references the effect changed: a move that gives an
	// item a new identity, such as an IMAP copy into the destination
	// mailbox, reports each item as its old reference and the new one. The
	// engine adds New to the stored message that holds Old, ahead of it, and
	// keeps Old. A provider whose references survive a move reports none.
	// Only an applied outcome's changes are used; an adapter reports them
	// again when it reconciles an applied move.
	ReferenceChanges []ReferenceChange
	Failure          *model.Error
	// Binding is, for an effect refused because the work item it reaches
	// is gone, the binding state the refusal shows:
	// model.WorkItemBindingDeleted (such as HTTP 410) or
	// model.WorkItemBindingInaccessible (such as HTTP 404). The engine moves
	// an active work item of the effect's conversation to it once the
	// operation settles rejected or failed. Any other value is ignored.
	Binding string
}
type Sender interface {
	Send(context.Context, SendRequest) (Outcome, error)
}

// Editor edits provider messages (model.CapabilityEdit). Deleter and
// Reactor follow the same rules.
//
// The target may be any message of the channel, sent through uma or not;
// the provider account's own rules decide what it may change. An adapter
// answers what the account cannot change, or a target the provider no
// longer has, with a final model.Rejected outcome whose failure code is
// model.Unauthorized, model.NotFound or model.Unsupported. It never falls
// back to posting new content, re-sending or any other effect.
type Editor interface {
	Edit(context.Context, EditRequest) (Outcome, error)
}

// Deleter deletes provider messages (model.CapabilityDelete); see Editor.
type Deleter interface {
	Delete(context.Context, DeleteRequest) (Outcome, error)
}

// Reactor adds and removes reactions (model.CapabilityReact); see Editor.
type Reactor interface {
	React(context.Context, ReactRequest) (Outcome, error)
}

// ReceiptRequest tells the sender of the provider message Effect.Target
// names, always set as for EditRequest, that it was delivered or read:
// Kind is model.ReceiptDelivered or model.ReceiptRead. The target is a
// message received from the provider, never one sent through uma.
type ReceiptRequest struct {
	Effect
	Kind string
}

// ReceiptSender sends delivery and read receipts (model.CapabilityReceipt).
// It follows Editor's rules: a receipt the account cannot send, or for a
// message the provider no longer has, is a final model.Rejected outcome,
// and a kind the provider has no receipt for is model.Rejected with
// model.Unsupported. A receipt changes nothing in uma once applied.
type ReceiptSender interface {
	SendReceipt(context.Context, ReceiptRequest) (Outcome, error)
}

// MembershipRequest adds Participant to the provider conversation on the
// effect's route, or removes it when Remove is set. Participant is the
// identity as the adapter reported it inbound: the route's channel, the
// identity's namespace and source ID, kind model.KindIdentity.
// Effect.Target is nil.
type MembershipRequest struct {
	Effect
	Participant model.ExternalRef
	Remove      bool
}

// MembershipChanger adds and removes conversation participants at the
// provider (model.CapabilityMembership). It follows Editor's rules: a
// change the account may not make, or for a conversation or participant
// the provider no longer has, is a final model.Rejected outcome, and it
// never falls back to another effect, such as posting a message that
// mentions the participant. Adding a present participant or removing an
// absent one is applied: the provider's membership is what was asked.
type MembershipChanger interface {
	ChangeMembership(context.Context, MembershipRequest) (Outcome, error)
}

// The provider conversation state interfaces, one per capability. Each
// effect's route is the conversation's reply route and Effect.Target is nil;
// the adapter fans out to the provider's items (a thread operation when the
// provider has one, else every message of the conversation on the channel).
// Each follows Editor's rules: a change the account may not make, or for a
// conversation the provider no longer has, is a final model.Rejected
// outcome, and an irreversible verb is model.Rejected with
// model.Unsupported. Applying the state already held is applied.

// ArchiveRequest archives the conversation on the effect's route, or
// unarchives it when Undo is set.
type ArchiveRequest struct {
	Effect
	Items []model.ExternalRef
	Undo  bool
	// Targeted marks Items as the messages the caller chose, a subset of
	// the conversation's: the adapter changes exactly those messages and
	// must not widen the change to a thread, folder or other grouping the
	// provider would otherwise change as a whole.
	Targeted bool
}

// Archiver archives and unarchives conversations (model.CapabilityArchive).
type Archiver interface {
	Archive(context.Context, ArchiveRequest) (Outcome, error)
}

// MoveRequest moves the conversation on the effect's route: to Destination,
// or by adding and removing the provider label IDs Add and Remove.
type MoveRequest struct {
	Effect
	Items       []model.ExternalRef
	Destination model.MoveDestination
	Add         []string
	Remove      []string
	// Targeted marks Items as the messages the caller chose, a subset of
	// the conversation's: the adapter changes exactly those messages and
	// must not widen the change to a thread, folder or other grouping the
	// provider would otherwise change as a whole.
	Targeted bool
}

// Mover moves conversations between folders or changes their labels
// (model.CapabilityMove).
type Mover interface {
	Move(context.Context, MoveRequest) (Outcome, error)
}

// ReadStateRequest marks the conversation on the effect's route read, or
// unread when Read is false.
type ReadStateRequest struct {
	Effect
	Items []model.ExternalRef
	Read  bool
	// Targeted marks Items as the messages the caller chose, a subset of
	// the conversation's: the adapter changes exactly those messages and
	// must not widen the change to a thread, folder or other grouping the
	// provider would otherwise change as a whole.
	Targeted bool
}

// ReadStateSetter marks conversations read and unread at the provider
// (model.CapabilityReadState).
type ReadStateSetter interface {
	SetReadState(context.Context, ReadStateRequest) (Outcome, error)
}

// FlagRequest sets or, with Undo, clears a flag of Kind (model.FlagStar or
// model.FlagImportant) on the conversation on the effect's route.
type FlagRequest struct {
	Effect
	Items []model.ExternalRef
	Kind  string
	Undo  bool
	// Targeted marks Items as the messages the caller chose, a subset of
	// the conversation's: the adapter changes exactly those messages and
	// must not widen the change to a thread, folder or other grouping the
	// provider would otherwise change as a whole.
	Targeted bool
}

// Flagger flags and unflags conversations (model.CapabilityFlag).
type Flagger interface {
	Flag(context.Context, FlagRequest) (Outcome, error)
}

// TrashRequest moves the conversation on the effect's route to the
// provider's trash, or restores it when Undo is set.
type TrashRequest struct {
	Effect
	Items []model.ExternalRef
	Undo  bool
	// Targeted marks Items as the messages the caller chose, a subset of
	// the conversation's: the adapter changes exactly those messages and
	// must not widen the change to a thread, folder or other grouping the
	// provider would otherwise change as a whole.
	Targeted bool
}

// Trasher trashes and restores conversations (model.CapabilityTrash). It
// never deletes at the peer.
type Trasher interface {
	Trash(context.Context, TrashRequest) (Outcome, error)
}

// HistoryRequest asks for one page of the provider's past messages on the
// channel, oldest first, occurring at or after Since and before Until: those
// of the conversation on Route when it is set, else the channel's. Until is
// the window's exclusive upper bound, judged by the time the provider
// reports for the message (for mail, when it was received); the zero time
// leaves the window open. The engine passes it through and does not check
// envelopes against it, as it does not check Since: a reader that ignores
// Until reads past it. Cursor is the Next of the page before, empty for the
// first; Limit caps the envelopes the page holds (at least 1).
//
// Stager, when set, is the engine's attachment staging for the channel, the
// one the receiver's sink offers: a reader that carries attachment bytes
// stages them through it as its inbound path does, under the same keys,
// bounds and failure handling, and names the staged attachments in the
// envelope's content. Staging is idempotent per channel and key, so reading
// a message again, or receiving it live after the import, adds no bytes. A
// reader that has no bytes to stage, or finds Stager nil, describes
// attachments only. Staging happens while the page is read, before the
// engine decides whether to import the envelope, so a message the engine
// then skips leaves its attachments staged and unreferenced.
type HistoryRequest struct {
	Route  *model.ExternalRef
	Since  time.Time
	Until  time.Time
	Cursor string
	Limit  int
	Stager AttachmentSink
}

// HistoryPage is one page of history. Envelopes are message envelopes
// (model.EnvelopeMessage or model.EnvelopeAutomatedMessage) as the adapter
// reports the same messages inbound, each with its provider message ID
// and destination, so the engine imports a message once whichever way it
// arrived. Next resumes after this page; empty, the window holds no more.
// Gap reports that the provider could not return part of the window, such
// as messages past its retention or out of the account's reach: the
// engine counts it rather than pretending the history is whole.
type HistoryPage struct {
	Envelopes []model.Envelope
	Next      string
	Gap       bool
}

// HistoryReader reads past messages from the provider
// (model.CapabilityHistory). A read changes nothing at the provider, so the
// engine may repeat it; a cursor the provider no longer accepts is
// model.Validation.
type HistoryReader interface {
	History(context.Context, HistoryRequest) (HistoryPage, error)
}

// ExtensionExecutor executes a registered extension effect
// (model.CapabilityExtensionExecute).
//
// Core and the adapter split the checks on the action. Core, before the
// effect is stored: the name and version are registered, as an effect
// definition (model.Unsupported or model.SchemaMismatch otherwise), and the
// data is valid JSON within the structured payload limit
// (model.Validation). Core treats the data as opaque past that, so fields
// it does not know reach Execute unchanged. The adapter owns the fields:
// which are required, their types and values. A Factory implementing
// ActionValidator is asked first, before the effect is stored: fields it
// refuses are a model.Validation error at the call with nothing stored. A
// Factory without it is not asked, so the effect is stored and the adapter
// validates in Execute, at the attempt: fields it refuses are a final
// model.Rejected outcome with a model.Validation failure, made before any
// provider call. Either way Execute validates, and never applies an action
// whose fields it did not validate.
type ExtensionExecutor interface {
	Execute(context.Context, ExecuteRequest) (Outcome, error)
}

// ExtensionReader answers a registered extension read
// (model.CapabilityExtensionQuery). Core checks the query like an effect's
// action (see ExtensionExecutor) and nothing is stored, so the adapter
// validates the query's fields in Query and answers a refusal with a
// model.Validation error. The result must carry the definition's result
// schema; core refuses anything else.
type ExtensionReader interface {
	Query(context.Context, model.QueryExtension) (model.Extension, error)
}

// ReconcileRequest asks for the provider outcome of an earlier attempt
// whose answer was lost. It carries the attempt's effect and what it sent
// or requested, so an adapter whose provider has no idempotency key can
// find the effect by its kind, content, recipient and time, and an adapter
// can compare a work item with the act requested of it.
type ReconcileRequest struct {
	Effect
	// Capability is the capability the attempt exercised, such as
	// model.CapabilitySend for a reply or model.CapabilityReact for a
	// reaction: the kind of effect to look for.
	Capability model.Capability
	// Content is the content the attempt sent, when its effect carried
	// any: a reply's or an ephemeral reply's message, or an edit's
	// replacement content.
	Content model.Content
	// Emoji and Remove are, for a reaction, the ReactRequest's.
	Emoji  string
	Remove bool
	// Receipt is, for a receipt, the ReceiptRequest's Kind; empty for
	// other effects.
	Receipt string
	// Participant is, for a membership change, the MembershipRequest's
	// Participant, with Remove its Remove; zero for other effects.
	Participant model.ExternalRef
	// Status and Reason are, for a work item act, the attempt's
	// WorkItemActRequest: the requested status and status reason, Reason
	// empty for the adapter's default. Both are empty for other effects.
	Status string
	Reason string
	// Action is, for an extension effect, the ExecuteRequest's Action: the
	// fields the attempt asked the provider to apply, so an adapter whose
	// provider keeps no marker on the resource can compare it with what
	// was requested. Zero for other effects.
	Action model.Extension
	// Items, Undo, Read, FlagKind, Destination, AddLabels and
	// RemoveLabels are, for a provider state effect (Capability archive,
	// move, read_state, flag or trash), the attempt's request fields: the
	// provider message references of the conversation, whether it undoes
	// (unarchive, unflag, untrash), the read value, the flag kind, and
	// the move's destination and label changes. Zero for
	// other effects. A move that is not idempotent reconciles by looking
	// in the destination: a conversation found there is applied, never
	// not found.
	Items        []model.ExternalRef
	Targeted     bool
	Undo         bool
	Read         bool
	FlagKind     string
	Destination  model.MoveDestination
	AddLabels    []string
	RemoveLabels []string
	// AttemptedAt is when the engine recorded the attempt, before calling
	// the provider: an effect the provider created earlier is not the
	// attempt's.
	AttemptedAt time.Time
}

type Reconciler interface {
	Reconcile(context.Context, ReconcileRequest) (Outcome, error)
}

// RefetchRequest names a provider resource whose inbound state the engine
// could not order: two observations carrying the same source revision (or
// source time) but different content, as a provider with coarse revisions
// reports for two changes within its revision granularity.
type RefetchRequest struct {
	// Target is the resource: a provider message (model.ProviderMessageRef)
	// or a work item reference (model.RefKindWorkItem) of the channel.
	Target model.ExternalRef
	// Destination is the destination of the observation that tied.
	Destination string
	// References are the references the tied observation carried, such
	// as the work item a message belongs to.
	References []model.ExternalRef
}

// RawRequest names the provider message whose whole copy is read.
type RawRequest struct {
	// Message is the message's provider reference (model.ProviderMessageRef)
	// on the adapter's channel.
	Message model.ExternalRef
}

// RawMessage is a provider message in the form the provider holds it. For
// mail it is the RFC 5322 message with its headers and every part, byte for
// byte as the provider serves it.
type RawMessage struct {
	// MediaType is the form of Bytes, "message/rfc822" for mail.
	MediaType string
	Bytes     []byte
}

// RawReader reads the provider's whole copy of one message
// (model.CapabilityRawRead). It is a read: nothing changes at the provider
// and the engine may repeat it. The adapter bounds the read by the size
// limit its inbound reads already keep, and a message over it is a
// model.Permanent error with the reason model.TooLarge, never a truncated
// copy. A message the provider no longer holds is model.NotFound. The
// engine stores nothing of the answer.
type RawReader interface {
	ReadRaw(context.Context, RawRequest) (RawMessage, error)
}

// Refetcher reads a resource's current state from the provider
// (model.CapabilityRefetch). It returns that state as one envelope, the
// way the adapter reports the resource inbound: a message_edited envelope
// for a message, a work_item envelope for a work item. The engine applies
// it as authoritative, whatever its source revision, and chooses its
// event ID itself. A read changes nothing at the provider, so the engine
// may repeat it.
type Refetcher interface {
	Refetch(context.Context, RefetchRequest) (model.Envelope, error)
}
type SignalRequest struct {
	Route     model.ExternalRef
	Kind      string
	Audience  model.Audience
	ExpiresAt time.Time
}
type Signaler interface {
	Signal(context.Context, SignalRequest) error
}
type EphemeralRequest struct {
	SendRequest
	Recipient model.ExternalRef
	ExpiresAt time.Time
}
type EphemeralSender interface {
	SendEphemeral(context.Context, EphemeralRequest) (Outcome, error)
}
type StreamRequest struct {
	Effect
	Mode     string
	Sequence uint64
	Content  model.Content
}
type StreamOutcome struct {
	Outcome
	ProviderStreamRef string
}
type Streamer interface {
	Start(context.Context, StreamRequest) (StreamOutcome, error)
	Update(context.Context, string, StreamRequest) (StreamOutcome, error)
	Finish(context.Context, string, StreamRequest) (StreamOutcome, error)
	Cancel(context.Context, string, Effect) (StreamOutcome, error)
}

// ExtensionDefinition registers one version of a typed extension: a read
// (Extensions.Query), an effect (Extensions.Execute) or an event kind.
// Versions of one name share Kind and AuthorizeAs.
//
// AuthorizeAs is a namespaced action or a builtin one, and the builtin
// actions are classified one by one, not by the shape of their names. An
// effect may authorize as a builtin action that writes through a provider
// resource (message.reply, message.note, work_item.act, task.act,
// conversation.transition, extension.execute and the like). Registration
// refuses an effect behind a read-only action (message.read, event.list,
// operation.read, extension.query and the other read actions) and behind a
// core-only action bound to a core flow: configuration, ingress, identity
// and contact data (contact.write, contact.merge, identity.*), operation
// recovery (operation.replay, operation.reconcile), subscriptions
// (subscription.*), attachment staging, streams, privacy and labels.
// Every builtin action has exactly one classification.
//
// An event-kind definition (Kind model.ExtensionKindEvent) lets an adapter
// declaring Name in Descriptor.Events emit it as a
// model.EnvelopeExtensionEvent envelope. The definition is the payload
// schema: InputSchema and ResultSchema stay empty. AuthorizeAs gates
// delivery: a subscriber receives the event only when it holds AuthorizeAs
// on the event's resource besides reading it, so a builtin AuthorizeAs
// must be a read-only action. Emitting needs nothing beyond
// model.ActionChannelReceive. The event log is immutable until retention
// and redaction never reaches it, so the adapter puts only provider
// references, identifiers and a change summary in event data: never
// message bodies, submitted values, attachment bytes or credentials.
type ExtensionDefinition struct {
	Name               string
	Version            uint32
	InputSchema        model.Extension
	ResultSchema       model.Extension
	RequiredCapability model.Capability
	AuthorizeAs        string
	Kind               string
}
