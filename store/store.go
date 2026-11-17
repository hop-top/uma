// Package store defines the storage provider interface: trusted, tenant-scoped
// persistence for domain records, the operation ledger, the event log,
// subscription checkpoints, work leases and adapter-owned state and
// recipient logs.
//
// It is not an authorization layer. The engine authorizes every request and
// decides mutation order before calling storage; storage supplies atomicity,
// isolation and constraint enforcement, and enforces tenant scope regardless
// of what the engine asks for.
//
// # Compatibility
//
// This interface is frozen at 101 methods: Store (3), Reader (55) and Tx
// (embeds Reader, adds 46). Both maintained backends implement it, and the
// review of its size and usability found no method to split or drop. Adding,
// removing or changing a method breaks every backend, so it needs a version
// bump and a migration note, even while the module is v0.x. A change to
// behaviour the conformance suites pin is breaking too. The storage schema
// is versioned by the backend and migrates forward only. See
// https://github.com/hop-top/uma/blob/next/docs/compatibility.md for the
// full policy. Applications
// using a maintained store never implement it themselves.
//
// # Method names
//
// Reader methods follow one naming rule, and new methods keep it:
//
//   - Get<Record> reads one domain record by its reference (GetChannel,
//     GetIdentity).
//   - A singular noun reads one record by a key other than a domain
//     reference (Identity by claim key, Protocol by channel and key,
//     Ingress by ID), one engine-owned record (Operation, Subscription,
//     Attachment, Stream), or one derived value (EventHead, WorkDepth).
//   - A plural noun pages a list (Channels, Attempts, Events) or returns a
//     bounded set (Candidates, MessagesByReference, QuarantinedWork).
//   - Resolve* returns the one record a lookup must match exactly.
//
// Tx methods are verbs: Put<Record> is a compare-and-set write, Claim* an
// idempotent create-or-return, and the rest name their transition.
//
// # Units of work
//
// [Store.View] runs a callback with a read-only [Reader]. [Store.Transact] runs
// a callback with a [Tx]: its reads see its own writes, and its writes,
// including appended events, commit together or not at all.
//
//   - A callback runs at most once per View or Transact call. The store never
//     retries it; the engine may retry a complete, side-effect-free unit.
//   - A callback error rolls the unit back and is returned unchanged. A
//     callback panic rolls it back and the panic continues after rollback.
//   - Handles are valid only during their callback and on its goroutine. A
//     handle used after its callback returned changes nothing and fails with a
//     *model.Error whose code is model.Permanent.
//   - Units do not nest: a callback never calls View or Transact, and never
//     performs a provider call or other external I/O.
//   - Units overlap. Units, in one tenant or many, may be inside their
//     callbacks at once, and a callback never waits for another unit's
//     callback to finish. A backend that holds a store-wide or tenant-wide
//     lock from the start of a callback to its commit is not valid: two
//     deliveries of one provider event rely on the second unit reaching its
//     claim while the first is still open. Conflicts surface at the
//     conflicting write or at commit, as model.Conflict for a violated
//     constraint or stale revision and as model.Transient when the unit lost
//     a race it did not cause; the loser stores nothing and the engine
//     retries the complete unit.
//   - The Reader passed to a View callback does not implement Tx.
//   - Transact returns only once the commit or rollback outcome is known where
//     the backend can know it, and never reports success after a failed
//     commit. An ambiguous commit is recovered through the persisted ingress
//     or operation key, not by re-running unrelated mutations.
//
// # Errors
//
// Every failure the store detects is a *model.Error, so callers branch on
// model.CodeOf:
//
//   - model.Validation: malformed input, including records that disagree with
//     their key or reference. A store checks references, keys, revisions and
//     the constraints it indexes on; of the model validators the maintained
//     stores apply only model.Label.Validate,
//     model.Conversation.ValidateLabels, model.Event.Validate,
//     model.IngressReceipt.Validate and model.Filter.Validate. Record
//     semantics such as model.Task.Validate belong to the engine, and a store
//     never rejects a task for them.
//   - model.NotFound: a missing record, a missing parent, or any reference owned
//     by a tenant other than the unit's. Foreign records are never confirmed.
//   - model.Conflict: a stale expected revision, a mismatched fingerprint, a
//     stale or foreign fence, a uniqueness violation, or a bounded read whose
//     complete result exceeds its limit.
//   - model.Transient: the unit could not be serialized against a concurrent
//     one; retrying the complete unit is safe.
//   - model.Unsupported: a primitive the backend does not provide.
//
// Context cancellation and deadline errors stay standard context errors.
//
// # Revisions
//
// The engine allocates revisions; storage compares them. Every write of a
// revisioned record carries the record holding its new, non-empty Revision
// and the expected current revision. An empty expected revision means the
// record must not exist yet. Comparison is byte-exact; a mismatch is
// model.Conflict and writes nothing.
//
// # Snapshots
//
// The store never retains caller memory and never returns memory it retains:
// values passed in are copied, and values returned are snapshots that share no
// slice, map, pointer or byte buffer with stored state.
//
// # Pages
//
// List reads take a filter or reference and a page request. The limit must be
// positive; a store may return fewer items than requested, including none,
// with a Next cursor. Complete reports that no further eligible item exists
// within the query's scan boundary. Cursors are opaque, valid only for the query
// that produced them, and stable across commits. A cursor a read cannot
// decode is model.Validation. A filter field a read does not support is
// model.Validation, never silently ignored.
//
// Each read that takes a model.Filter honours these fields and no others:
//
//   - Channels, Labels: none.
//   - Inboxes: Inboxes.
//   - Contacts: Kinds.
//   - Identities: Inboxes, Contact.
//   - Conversations: Inboxes, Statuses, Labels, Assignee (Actor or None;
//     Self is model.Validation, resolved by the engine) and Snoozed: a
//     conversation's Snooze is in effect while Until is after the store
//     clock's now and no live inbound message of it (as InboundMessages
//     selects them) that was not imported (empty Message.ImportedBy) has
//     a RecordedAt after At, SnoozeEnded (a Snooze is stored but no
//     longer in effect by that rule) and Linked (holding any link, or none) and
//     Unread (Actor required: a message that model.ReadMarker.Unread
//     counts for the actor's marker, or any such message when the actor
//     has none; an imported message never counts).
//   - Messages: Inboxes, Conversation, Labels, Task and the search fields
//     Text, Since, Before and Attachments.
//   - WorkItems: Inboxes, Conversation, Statuses.
//   - Tasks: Inboxes, Conversation, Statuses, Identity.
//   - Interactions: Inboxes, Conversation, Identity.
//   - Events: Inboxes, Conversation (the conversation EventConversation
//     resolves; an event with none never matches), Kinds and Since (on
//     Event.RecordedAt; see Reader.Events).
//   - Operations: Inboxes, Conversation, Statuses (operation states), Since
//     and Before (on the time the operation was last written).
//
// PutSubscription stores its filter under the Events rule without
// Conversation and Since: Inboxes and Kinds only, any other field is
// model.Validation.
//
// A filter that fails model.Filter.Validate is model.Validation, and a
// reference in it that another tenant owns is model.NotFound.
package store

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Properties describes a backend without connecting to it.
type Properties struct {
	// Durable reports crash and restart durability of committed units.
	Durable bool
	// SchemaVersion is the storage schema revision the backend implements.
	SchemaVersion uint32
	// Backend names the implementation, e.g. "memory" or "postgres".
	Backend string
}

// Store opens tenant-scoped units of work.
type Store interface {
	// Properties reports the backend description without I/O.
	Properties() Properties
	// View runs fn once with a read-only Reader scoped to tenant.
	View(ctx context.Context, tenant model.TenantID, fn func(Reader) error) error
	// Transact runs fn once with a Tx scoped to tenant and commits its writes
	// when fn returns nil.
	Transact(ctx context.Context, tenant model.TenantID, fn func(Tx) error) error
}

// Reader is the read side of a unit of work. Every reference must belong to
// the unit's tenant. Reader queries are trusted storage reads, not authorized
// application queries: the engine authorizes before exposing results.
type Reader interface {
	GetChannel(context.Context, model.ChannelRef) (model.Channel, error)
	GetInbox(context.Context, model.InboxRef) (model.Inbox, error)
	GetContact(context.Context, model.ContactRef) (model.Contact, error)
	GetConversation(context.Context, model.ConversationRef) (model.Conversation, error)
	GetMessage(context.Context, model.MessageRef) (model.Message, error)
	GetWorkItem(context.Context, model.WorkItemRef) (model.WorkItem, error)
	GetTask(context.Context, model.TaskRef) (model.Task, error)
	// TaskByReference returns the task of inbox whose External is ref,
	// compared field by field. PutTask keeps a non-zero External unique
	// within an inbox, so at most one task matches.
	TaskByReference(ctx context.Context, inbox model.InboxRef, ref model.ExternalRef) (model.Task, error)
	// GetIdentity returns the identity stored under ref. Identity reads the
	// same record by its claim key.
	GetIdentity(context.Context, model.IdentityRef) (model.Identity, error)
	// Protocol returns the adapter-owned state stored under key for one
	// channel instance.
	Protocol(ctx context.Context, channel model.ChannelRef, key string) (ProtocolState, error)
	// RecipientLogEntries returns the retained entries of one recipient log
	// with a sequence above after, oldest first, at most limit, which must
	// be positive; a store may return fewer. A log never appended to, or
	// whose channel is not stored, has none. Entries trimmed by
	// AppendRecipientLog are gone: the first entry returned then has a
	// sequence above after+1.
	RecipientLogEntries(ctx context.Context, key RecipientLogKey, after uint64, limit int) ([]RecipientLogEntry, error)

	Channels(context.Context, model.ListRequest) (model.Page[model.Channel], error)
	Inboxes(context.Context, model.ListRequest) (model.Page[model.Inbox], error)
	Contacts(context.Context, model.ListRequest) (model.Page[model.Contact], error)
	Identities(context.Context, model.ListRequest) (model.Page[model.Identity], error)
	// Conversations honours Filter.Inboxes, Statuses and Labels. A label
	// selector matches a conversation's Labels as a conjunction of its
	// non-empty parts: Any holds at least one, All holds every one, None
	// holds none. A selector label is a set member only: a label of the
	// tenant that is not stored matches no conversation and is never read.
	Conversations(context.Context, model.ListRequest) (model.Page[model.Conversation], error)
	// Messages pages messages in recording order. It honours
	// Filter.Inboxes, Filter.Conversation, Filter.Labels (evaluated on the
	// labels the message's conversation holds, as Conversations does, by
	// join: a label change shows at once and no label is copied onto a
	// message), Filter.Task (the messages bound
	// to that task, model.Message.Task: the task's message history) and,
	// alone among the reads, the message search fields Text, Since, Before and
	// Attachments, evaluated in storage under the rules of model's
	// search documentation: Text against the message's text-part tokens
	// (model.Content.SearchTokens; none once Redacted or Deleted), Since
	// and Before on OccurredAt, Has on the attachment parts naming this
	// tenant's attachments, MediaTypes on the current attachment record's
	// media type (parameters stripped, ASCII spaces and tabs trimmed,
	// ASCII-lowercased; a missing or AttachmentRedacted record never
	// matches). A write, rewrite or Redact changes what matches within its
	// unit. Search changes neither the recording order nor the cursors.
	Messages(context.Context, model.ListRequest) (model.Page[model.Message], error)
	// InboundMessages pages the live inbound messages of conversation, newest
	// first: those with no Origin (not authored by uma) and no EchoOf (not
	// the provider's copy of one that was), neither Redacted nor Deleted.
	// Newest is by recording order, the reverse of Messages: a rewrite keeps
	// a message's place. Audience is not read; the engine authorizes what it
	// returns. The cursor is the position of the last message returned; a
	// page continues strictly older, so a message recorded after the first
	// page is never in a later one. A cursor is valid only for the method
	// that returned it. A conversation without inbound messages, or not
	// stored, is an empty complete page. It exists so a reader of the most
	// recent inbound message does not list the whole history to find it.
	InboundMessages(ctx context.Context, conversation model.ConversationRef, page model.PageRequest) (model.Page[model.Message], error)
	WorkItems(context.Context, model.ListRequest) (model.Page[model.WorkItem], error)
	// Tasks pages tasks most recent status first: StatusAt descending,
	// then ID descending. The cursor is the position of the last task
	// returned, never an offset, so tasks written after a page was read
	// never shift the next one; a task whose StatusAt changes moves. It
	// honours Filter.Inboxes, Conversation, Statuses (task states) and
	// Identity, which keeps the tasks of conversations whose Owner is that
	// identity: a principal's tasks, per agent context.
	Tasks(context.Context, model.ListRequest) (model.Page[model.Task], error)
	// Replies pages, in recording order, the messages whose InReplyTo is
	// parent: the replies answering it, uma-authored or inbound. The parent need
	// not exist.
	Replies(ctx context.Context, parent model.MessageRef, page model.PageRequest) (model.Page[model.Message], error)

	// GetLabel reads one label definition. LabelByName returns the tenant's
	// label whose name has the same model.LabelNameKey as name; an empty
	// name is model.Validation. Labels pages the tenant's labels in
	// insertion order and honours no filter field.
	GetLabel(context.Context, model.LabelRef) (model.Label, error)
	LabelByName(ctx context.Context, name string) (model.Label, error)
	Labels(context.Context, model.ListRequest) (model.Page[model.Label], error)
	// ReadMarker returns actor's read marker on conversation: model.NotFound
	// when the actor has none, or the conversation is not stored.
	ReadMarker(ctx context.Context, conversation model.ConversationRef, actor model.ActorID) (model.ReadMarker, error)

	Operation(context.Context, model.OperationRef) (OperationRecord, error)
	// OperationByKey returns the operation claimed under key, as
	// Operation would by its reference. A key never claimed, or whose
	// operation was purged, is model.NotFound, as is a key naming another
	// tenant than the unit's. A key without a Kind or an ID is
	// model.Validation.
	OperationByKey(context.Context, OperationKey) (OperationRecord, error)
	// Operations pages the operation ledger most recently written first:
	// write time descending, then ID descending. The write time is the
	// store-clock time of the operation's latest write (its claim, state
	// change or settling), compared to the microsecond; each record returned
	// shows it as Operation.UpdatedAt. The cursor is the
	// position of the last operation returned, never an offset, so
	// operations accepted after a page was read never shift the next one;
	// an operation written again moves to the front, where a page already
	// read will not see it. It honours Filter.Inboxes and Conversation (the
	// operation's OperationRecord.Scope), Statuses (operation states) and
	// the window Since (inclusive) and Before (exclusive) on the write
	// time. An operation with no inbox matches no inbox filter, one with no
	// conversation no conversation filter. Like every Reader read it
	// authorizes nothing.
	Operations(context.Context, model.ListRequest) (model.Page[OperationRecord], error)
	// Attempts pages one operation's attempt evidence in append order.
	Attempts(context.Context, model.OperationRef, model.PageRequest) (model.Page[AttemptEvidence], error)
	Ingress(context.Context, model.ID) (IngressRecord, error)
	Stream(context.Context, model.StreamRef) (model.Stream, error)
	// StreamsOf returns the streams of conversation that are not terminal
	// (model.StreamOpening, model.StreamOpen and model.StreamEnding),
	// ordered by ID. Terminal streams are never returned.
	StreamsOf(ctx context.Context, conversation model.ConversationRef) ([]model.Stream, error)
	// Interaction returns the interaction stored under ref, never its
	// response token.
	Interaction(context.Context, model.InteractionRef) (model.Interaction, error)
	// Interactions pages interactions in recording order, filtered by
	// Inboxes, Conversation and Identity (the actor's identity), never
	// with their response tokens.
	Interactions(context.Context, model.ListRequest) (model.Page[model.Interaction], error)
	// UnlinkedInteractions pages, in recording order, the interactions of
	// inbox whose MessageExternal is message, compared field by field, and
	// whose Message is unset: interactions on a provider message that no
	// local message held when they were recorded, such as a control pressed
	// before the send that posted it recorded its provider message ID. It
	// never returns their response tokens. LinkInteraction removes an
	// interaction from the set, within its unit too.
	UnlinkedInteractions(ctx context.Context, inbox model.InboxRef, message model.ExternalRef, page model.PageRequest) (model.Page[model.Interaction], error)
	// InteractionToken returns the response token stored with the
	// interaction ref names: the only read that returns it, for the
	// effector at attempt time.
	InteractionToken(context.Context, model.InteractionRef) (ResponseToken, error)
	// Lease returns the unexpired lease on resource, judged by the store's
	// clock; a resource never leased, released or past its lease is
	// model.NotFound.
	Lease(ctx context.Context, resource model.Ref) (Lease, error)
	// Candidates returns the conversations claimed under key that are not
	// closed, plus the newest closed one by CreatedAt then ID, ordered by
	// CreatedAt then ID. Older closed conversations are never candidates and
	// never count toward limit, so closed history alone cannot exhaust it.
	// It never truncates: when more than limit conversations are eligible,
	// the newest closed one included, it fails with model.Conflict, because a
	// partial candidate set could change a deterministic selection.
	Candidates(ctx context.Context, key SelectionKey, limit int) ([]model.Conversation, error)
	// ResolveDestination returns the one inbox bound to channel that lists
	// destination. More than one match is model.Conflict.
	ResolveDestination(ctx context.Context, channel model.ChannelRef, destination string) (model.Inbox, error)
	// ResolveChannelKey returns the tenant's channel whose Key is key, and
	// ResolveInboxKey the tenant's inbox whose Key is key. An empty key is
	// model.Validation. PutChannel and PutInbox keep a non-empty key unique
	// among the tenant's channels and inboxes respectively: a key another
	// record holds is model.Conflict.
	ResolveChannelKey(ctx context.Context, key string) (model.Channel, error)
	ResolveInboxKey(ctx context.Context, key string) (model.Inbox, error)
	// MessagesByReference returns every message of inbox whose External
	// holds ref, compared field by field, ordered by RecordedAt then ID. Like
	// Candidates it never truncates: more than limit matches is
	// model.Conflict.
	MessagesByReference(ctx context.Context, inbox model.InboxRef, ref model.ExternalRef, limit int) ([]model.Message, error)
	// MessagesByAttachment pages, in recording order, the messages whose
	// current content parts name the attachment ref, whatever their inbox:
	// the messages that hold it. A message stops holding an attachment
	// once a write or Redact drops the part. The attachment record need
	// not exist.
	MessagesByAttachment(ctx context.Context, ref model.AttachmentRef, page model.PageRequest) (model.Page[model.Message], error)
	Identity(context.Context, model.IdentityKey) (model.Identity, error)
	Subscription(context.Context, model.SubscriptionRef) (SubscriptionRecord, error)
	// Events pages committed events after cursor in commit order, filtered
	// by Inboxes, Conversation, Kinds and Since. An event is in an inbox
	// when it is scoped to it or is about the inbox itself, and in a
	// conversation when EventConversation resolves that conversation for
	// it. Since keeps the events whose RecordedAt is at or after it.
	// RecordedAt follows commit order only roughly (an event's unit sets it
	// before commit), so a read must not stop at the first event below
	// Since; it may instead start at the first position at which the
	// running maximum of RecordedAt over the log reaches Since, since no
	// earlier event can match, and compare each event after it. Next and
	// Complete are unchanged by that start: a page that starts past cursor
	// still reports the position after the last scanned event, and the
	// head when complete. An empty cursor is the beginning of the log. A
	// cursor before EventBase, the empty one included once events were
	// purged, fails with model.CursorExpired, never by tailing. A cursor
	// beyond the last committed event is model.Validation. Next is the
	// position after the last scanned event, also on a complete page, so it
	// can be stored as a checkpoint.
	Events(ctx context.Context, filter model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error)
	// EventHead returns the position after the last committed event in the
	// unit's snapshot, as an Events page would report it in Next; an empty
	// log reports EventBase. Within a Tx it includes the unit's own appends.
	EventHead(ctx context.Context) (model.Cursor, error)
	// EventBase returns the earliest cursor Events accepts: the position
	// after the last purged event, recorded by Purge, or "" when no event
	// was ever purged.
	EventBase(ctx context.Context) (model.Cursor, error)
	// Event returns the event ref names while the log retains it, as stored.
	// An event the log never held, one purged by retention and a ref of
	// another tenant are all model.NotFound. Who may read the event is the caller's to decide.
	Event(ctx context.Context, ref model.EventRef) (model.Event, error)
	// WorkDepth counts the stored work items of kind by state.
	WorkDepth(ctx context.Context, kind string) (WorkDepth, error)
	// QuarantinedWork returns the quarantined work items on resource in
	// enqueue order.
	QuarantinedWork(ctx context.Context, resource model.Ref) ([]Work, error)
	Attachment(context.Context, model.AttachmentRef) (AttachmentRecord, error)
	Export(context.Context, model.ExportRef) (ExportRecord, error)
	// Staging returns the staging intent recorded for key.
	Staging(ctx context.Context, key ports.BlobKey) (StagingIntent, error)
	// IdentityLinks pages link history in append order, tombstones
	// included.
	IdentityLinks(context.Context, LinkFilter, model.PageRequest) (model.Page[LinkRecord], error)
}

// Tx is the read-write side of a unit of work. Writes become visible to other
// units only when the unit commits.
type Tx interface {
	Reader

	PutChannel(ctx context.Context, channel model.Channel, expected model.Revision) error
	PutInbox(ctx context.Context, inbox model.Inbox, expected model.Revision) error
	PutContact(ctx context.Context, contact model.Contact, expected model.Revision) error
	// PutConversation writes a conversation. Its Labels are its label
	// assignments, so assigning or removing a label is a conversation write
	// under its revision. Labels must satisfy
	// model.Conversation.ValidateLabels and name stored labels: a missing
	// label is model.NotFound.
	PutConversation(ctx context.Context, conversation model.Conversation, expected model.Revision) error
	// PutMessage writes a message under an existing conversation. Its
	// Task and InReplyTo, when set, must belong to that conversation, else
	// model.Validation.
	PutMessage(ctx context.Context, message model.Message, expected model.Revision) error
	// PutWorkItem writes a work item. A conversation holds at most one: a
	// new work item in a conversation that already holds another is
	// model.Conflict.
	PutWorkItem(ctx context.Context, item model.WorkItem, expected model.Revision) error
	// PutTask writes a task under an existing conversation. A non-zero
	// External is unique within the task's inbox: one another task holds
	// is model.Conflict. The store keeps the task listed under its
	// conversation's Owner (Reader.Tasks), also when PutConversation later
	// changes the owner.
	PutTask(ctx context.Context, task model.Task, expected model.Revision) error
	// PutLabel writes a label definition. Names are unique among the
	// tenant's labels under model.LabelNameKey: a name whose key another
	// label holds is model.Conflict.
	PutLabel(ctx context.Context, label model.Label, expected model.Revision) error
	// DeleteLabel removes a label definition, compare-and-set on expected,
	// which is required. A label any conversation holds is model.Conflict:
	// assignments are removed through PutConversation first.
	DeleteLabel(ctx context.Context, ref model.LabelRef, expected model.Revision) error
	// PutReadMarker writes one actor's read marker on a stored conversation
	// (model.NotFound otherwise), keyed by conversation and actor,
	// compare-and-set on expected. It never changes the conversation.
	PutReadMarker(ctx context.Context, marker model.ReadMarker, expected model.Revision) error
	// PutAttachment writes an attachment record. Its inbox must be stored
	// (model.NotFound otherwise) and its blob key, when set, must be in the
	// unit's tenant.
	PutAttachment(ctx context.Context, record AttachmentRecord, expected model.Revision) error
	// PutExport writes an export record. Its blob key must be in the
	// unit's tenant.
	PutExport(ctx context.Context, record ExportRecord, expected model.Revision) error
	// RecordStaging stores a staging intent, stamped with the store's
	// clock. The key must be in the unit's tenant and the intent not
	// abandoned; a key already recorded is model.Conflict. A malformed key
	// or an intent naming no owner or both owners is model.Validation,
	// checked before the tenant: only a well-formed key of another tenant
	// is model.NotFound. ClearStaging checks its key the same way.
	RecordStaging(ctx context.Context, intent StagingIntent) error
	// BindStaging removes key's intent as part of the unit that binds the
	// key to its attachment record. A key without an intent is
	// model.NotFound and an abandoned one model.Conflict: the sweep may
	// already be deleting its bytes.
	BindStaging(ctx context.Context, key ports.BlobKey) error
	// ClearStaging removes key's intent, abandoned or not, once its bytes
	// are deleted. A key without an intent succeeds.
	ClearStaging(ctx context.Context, key ports.BlobKey) error
	// AbandonStaging marks abandoned, and returns in recording order, up
	// to sweep.Limit intents that are abandoned already or were recorded
	// before sweep.Before, judged by the store's clock. Limit must be
	// positive and Before set, else model.Validation. An abandoned
	// intent stays until ClearStaging, so a sweep that failed to delete
	// its bytes finds it again.
	AbandonStaging(ctx context.Context, sweep StagingSweep) (StagingBatch, error)
	PutStream(ctx context.Context, stream model.Stream, expected model.Revision) error
	// PutInteraction stores a new interaction with its ingress ID and
	// response token. It only creates: a ref already stored is
	// model.Conflict, so no replace path carries the token back. The
	// interaction's conversation must be stored (model.NotFound). Its
	// references must be well formed and belong to the unit's tenant
	// (foreign ones are model.NotFound), and the record needs a declared
	// Kind, a Revision, a valid Ingress ID and, whenever a token is held,
	// TokenExpiresAt, else model.Validation.
	PutInteraction(ctx context.Context, record InteractionRecord) error
	// RecordInteractionResponse records one accepted response and
	// returns the interaction as stored under claim.Revision. An initial
	// claim sets Response.Initial when it is unset and is model.Conflict
	// otherwise; a follow-up adds one to Response.FollowUps. A redacted
	// interaction is model.Conflict. Neither compares revisions, so a
	// follow-up never fails an initial claim; claim.Revision must be set
	// and differ from the stored one, else model.Validation, and an
	// unknown interaction is model.NotFound.
	RecordInteractionResponse(ctx context.Context, claim ResponseClaim) (model.Interaction, error)
	// LinkInteraction sets the Message of an interaction that names a
	// provider message (MessageExternal) and no local message yet to
	// link.Message, under link.Revision, and returns the interaction as
	// stored. Once linked, the interaction leaves UnlinkedInteractions. An
	// interaction already naming link.Message is returned unchanged, so a
	// rerun links nothing twice; one naming another message, or naming no
	// provider message, is model.Conflict. A redacted interaction is
	// linked like any other: redaction keeps references. It compares no
	// revision; link.Revision must be set and differ from the stored one,
	// and link.Message must be in the interaction's inbox, else
	// model.Validation. An unknown interaction is model.NotFound. The
	// response token is untouched.
	LinkInteraction(ctx context.Context, link InteractionLink) (model.Interaction, error)
	// PutProtocol is a compare-and-set on one channel instance's key.
	PutProtocol(ctx context.Context, state ProtocolState, expected model.Revision) error
	// AppendRecipientLog stores value as the next entry of key's log and
	// returns its sequence: 1, 2, 3 and so on per key, without gaps among
	// committed units. It then keeps the latest retain entries of the log
	// and deletes older ones; retain must be positive. The channel must be
	// stored (model.NotFound otherwise). Value.Name is required and
	// Value.Data, when present, must be valid JSON. Concurrent appends to
	// one key serialize: a unit that loses the race fails with
	// model.Transient and stores nothing.
	AppendRecipientLog(ctx context.Context, key RecipientLogKey, value model.Extension, retain int) (uint64, error)
	// EraseRecipientLog deletes every retained entry of key's log and
	// returns the number deleted. The log keeps its last sequence, so a
	// later AppendRecipientLog continues after it and a reader holding an
	// older cursor sees a gap, as after a trim, never a reused sequence.
	// A log never appended to, one already erased and one whose channel is
	// not stored have no entries: it returns 0 and no error. The key must
	// be valid (model.Validation otherwise). Privacy erasure of a contact
	// or identity calls it, in the erasure's unit, for the log addressed
	// by each identity's channel, Namespace and SourceID.
	EraseRecipientLog(ctx context.Context, key RecipientLogKey) (int, error)
	PutSubscription(ctx context.Context, record SubscriptionRecord, expected model.Revision) error

	// ClaimIdentity stores identity under key unless the key is claimed, and
	// returns the stored identity and whether this call created it. An
	// existing claim is returned unchanged. Key and identity must agree.
	// Identity.Contact need not exist yet: the engine may write the
	// provisional contact after a successful claim in the same unit.
	ClaimIdentity(ctx context.Context, key model.IdentityKey, identity model.Identity) (model.Identity, bool, error)
	// LinkIdentity appends a link history record and moves the identity's
	// contact pointer from record.From to record.To, compare-and-set on
	// record.RevisionBefore. The identity takes record.RevisionAfter and
	// record.Assurance; no contact record or revision changes. A non-empty
	// evidence (Verifier, Reference) pair is single-use across records.
	// record.From must be the identity's current contact and
	// record.Supersedes the ID of its latest link record, empty for the
	// first, else model.Validation.
	LinkIdentity(ctx context.Context, record LinkRecord) error
	// RedactLinks tombstones every link history record of identity: each
	// loses its evidence Facts and takes Redacted, and keeps every other
	// field, the evidence Verifier and Reference included, so a consumed
	// (Verifier, Reference) pair stays consumed. It changes no identity or
	// contact, and returns the number of records it tombstoned; records
	// already tombstoned are left as they are and not counted. An identity
	// that does not exist is model.NotFound. Redact never touches link
	// records.
	RedactLinks(ctx context.Context, identity model.IdentityRef) (int, error)
	// ClaimConversation stores the claim's conversation unless the claim's
	// (Selection, CreateKey) is claimed, and returns the conversation claimed
	// under it and whether this call created it. The conversation joins the
	// Selection candidate group. The conversation must be in the
	// Selection's inbox and carry the Selection's Mode, else
	// model.Validation.
	ClaimConversation(ctx context.Context, claim ConversationClaim) (model.Conversation, bool, error)
	// ClaimMessage stores the claim's message, as PutMessage with an empty
	// expected revision, unless the message's inbox already claimed
	// claim.Reference, and returns the message claimed under it and
	// whether this call created it. The reference must be one of the
	// message's External. A claim lasts as long as its message.
	ClaimMessage(ctx context.Context, claim MessageClaim) (model.Message, bool, error)

	// ClaimIngress stores record unless its (Channel, DedupKey) is claimed,
	// and returns the stored record and whether this call created it.
	ClaimIngress(ctx context.Context, record IngressRecord) (IngressRecord, bool, error)
	// CompleteIngress records the receipt of a claimed ingress record once.
	// Repeating an equal receipt (model.IngressReceipt.Equal) is a no-op; a
	// different one is a conflict. Its references must be in the unit's
	// tenant, and come back unchanged from Ingress and from a duplicate
	// ClaimIngress.
	CompleteIngress(ctx context.Context, id model.ID, receipt model.IngressReceipt) error

	// ClaimOperation stores record unless its Key is claimed, and returns the
	// stored record and whether this call created it. An existing claim with
	// a different fingerprint version or fingerprint is model.Conflict. The
	// record returned shows the write time (see OperationRecord); one the
	// caller put in record is ignored.
	ClaimOperation(ctx context.Context, record OperationRecord) (OperationRecord, bool, error)
	// AppendAttemptEvidence appends evidence for an existing operation even
	// when its fence is no longer current. Appending never authorizes an
	// operation state change.
	AppendAttemptEvidence(ctx context.Context, evidence AttemptEvidence) error
	// UpdateOperation replaces the operation state, compare-and-set on
	// expected, and moves its write time to the store clock's now; an
	// UpdatedAt in operation is ignored. A non-zero fence must be the current, unexpired lease on the
	// operation; a zero fence requires that no unexpired lease is held on it.
	UpdateOperation(ctx context.Context, operation model.Operation, expected model.Revision, fence Fence) error

	// AppendEvents appends events in order within the unit. They become
	// readable only when the unit commits; no callback or publication runs
	// before commit. Event IDs are unique per tenant.
	AppendEvents(ctx context.Context, events []model.Event) error
	// AdvanceCheckpoint moves a subscription checkpoint from Prior to Next
	// under the subscription's current lease and clears its pending batch.
	AdvanceCheckpoint(ctx context.Context, advance CheckpointAdvance) error

	// EnqueueWork records pending work in the unit, so it becomes claimable
	// only if the unit commits. Fence, Until and Claims are set by claims
	// and must be zero (model.Validation); an ID already enqueued is
	// model.Conflict.
	EnqueueWork(ctx context.Context, work Work) error
	// ClaimWork leases up to claim.Limit pending work items of claim.Kinds,
	// in enqueue order, whose NotBefore has passed and whose resources hold
	// no unexpired lease. Quarantined items are never claimed. Each
	// returned item carries the fence of its resource lease and its Claims
	// count, this claim included. Claims are at-least-once: an expired
	// lease makes the item claimable again under a higher generation.
	//
	// A positive claim.MaxInFlight caps, per kind, the items holding an
	// unexpired claim at once, those claimed earlier by any owner
	// included. When the cap held back a claimable item and the call
	// claimed nothing, it fails with model.Unavailable rather than return
	// an empty claim; a claim the cap only shortened succeeds.
	ClaimWork(ctx context.Context, claim WorkClaim) ([]Work, error)
	// CompleteWork removes a claimed work item and releases its resource
	// lease. work.Fence must be the current, unexpired lease.
	CompleteWork(ctx context.Context, work Work) error
	// QuarantineWork takes a claimed work item out of the claimable queue
	// and releases its resource lease. work.Fence must be the current,
	// unexpired lease. The item stays stored until RequeueWork.
	QuarantineWork(ctx context.Context, work Work) error
	// RequeueWork returns a quarantined work item to the queue, claimable
	// at once with its Claims count reset. Requeueing an item that is not
	// quarantined is model.Conflict.
	RequeueWork(ctx context.Context, id model.ID) error

	// AcquireLease leases a resource that holds no unexpired lease, under the
	// resource's next generation.
	AcquireLease(ctx context.Context, request LeaseRequest) (Lease, error)
	// RenewLease extends a current, unexpired lease to lease.Until.
	RenewLease(ctx context.Context, lease Lease) (Lease, error)
	// ReleaseLease drops a lease whose fence is current.
	ReleaseLease(ctx context.Context, lease Lease) error

	// Redact replaces one message, interaction or attachment record, and the
	// records holding copies of its content, with tombstones in the unit.
	// Expected must equal the target's revision and Revision,
	// engine-allocated, becomes the new revision of every message and
	// attachment record it rewrites; a mismatch is model.Conflict. Exactly
	// one target, a non-empty Expected and a non-empty Revision that differs
	// from it are required, else model.Validation. Every check precedes the
	// first write, so a failed call writes nothing.
	//
	//   - A message keeps Ref, Author, Audience, External, Task,
	//     InReplyTo, times, Origin, ImportedBy, Source and its
	//     CoreNamespace metadata; its content and other metadata are
	//     cleared and Redacted is set. Every other field stays as stored:
	//     Deleted, Reactions, ReactionsComplete, LastEdit, EchoOf and
	//     InResponseTo included. Reactions and LastEdit are not content:
	//     they record who reacted and who edited, and carry no message text.
	//   - An interaction keeps its references, actor, kind, identifiers,
	//     response state and token expiry; its field data, arguments and
	//     response token are cleared and Redacted is set. The ingress
	//     record that created it is scrubbed as an ingress record naming
	//     a message is.
	//   - The attachment target, and each stored attachment the message's
	//     parts reference, keeps Ref, Audience, StagingOperation and
	//     Stager, takes State AttachmentRedacted and loses its other
	//     metadata and its blob and provider locators. Each unbound blob
	//     key gets a staging intent naming the attachment, so the retention
	//     sweep deletes the bytes if the caller does not; the keys are
	//     returned for the caller to delete after commit and then clear.
	//   - Ingress records naming one of the message's provider message IDs
	//     (model.ProviderMessageRef) on that ID's channel keep their ID,
	//     channel, dedup key, receipt and envelope routing; envelope
	//     content, raw payload data and interaction field data are cleared.
	//   - The message's originating operation keeps its key, fingerprint,
	//     state and revision and loses its intent data (Command.Data; the
	//     command's name and version stay). A work item
	//     referring to it, or an unexpired lease on it, is model.Conflict:
	//     a worker would still read that intent.
	//   - Events and attempt evidence carry references and summaries only
	//     and are left unchanged. Purge applies to tombstones as to any
	//     record, so replay correlation lasts as long as its family's
	//     retention.
	Redact(ctx context.Context, request Redaction) (RedactResult, error)

	// Purge deletes up to request.Limit records of request.Family that the
	// store wrote before request.Before, judged by the store's clock, and
	// reports whether none is left. The families keep these rules:
	//
	//   - FamilyEvents deletes a prefix of the log only, in commit order,
	//     stopping at the first event written at or after Before, and
	//     records the position after the last deleted event as EventBase.
	//   - FamilyOperations deletes operation records, with their attempt
	//     evidence, last written before Before that no work item, pending,
	//     claimed or quarantined, refers to and that hold no unexpired lease.
	//   - FamilyAttempts deletes attempt evidence appended before Before
	//     of operations that no work item refers to and that hold no
	//     unexpired lease.
	//   - FamilyIngress deletes ingress records claimed before Before, and
	//     their deduplication keys with them.
	//   - FamilyExports deletes export records written before Before. In
	//     the same unit it records a staging intent naming each deleted
	//     export's blob key, so the retention sweep deletes the bytes, and
	//     it reports the keys in Blobs for the caller to delete and clear.
	//   - FamilyContacts deletes contacts no identity points at whose
	//     latest link record with the contact as From was written before
	//     Before. A contact no link record ever left, such as a
	//     provisional contact or one only linked to, is never deleted.
	//     Link records are kept and keep naming deleted contacts.
	Purge(ctx context.Context, request PurgeRequest) (PurgeResult, error)
}

// Redaction asks Redact to tombstone one target: Message, Interaction or
// Attachment, exactly one set. Expected is the target's current revision
// and Revision the new one.
type Redaction struct {
	Message     *model.MessageRef
	Interaction *model.InteractionRef
	Attachment  *model.AttachmentRef
	Expected    model.Revision
	Revision    model.Revision
}

// RedactResult reports one Redact call: the blob keys of the attachment
// records it tombstoned, which no record binds any more.
type RedactResult struct {
	Blobs []ports.BlobKey
}

// Record families Purge deletes from.
const (
	FamilyEvents     = "events"
	FamilyOperations = "operations"
	FamilyAttempts   = "attempts"
	FamilyIngress    = "ingress"
	FamilyExports    = "exports"
	FamilyContacts   = "contacts"
	// FamilyInteractionTokens clears the response tokens of interactions
	// whose token expired before Before. It deletes no record, changes no
	// revision and emits nothing. Interaction records themselves have no
	// purge family.
	FamilyInteractionTokens = "interaction_tokens"
)

// PurgeRequest asks Purge for up to Limit records of Family written before
// Before. Limit must be positive.
type PurgeRequest struct {
	Family string
	Before time.Time
	Limit  int
}

// PurgeResult reports one Purge call. Complete reports that no further
// record of the family is eligible. EventBase is, for FamilyEvents, the
// earliest cursor Events accepts after the call. Blobs is, for
// FamilyExports, the blob keys of the deleted exports.
type PurgeResult struct {
	Purged    int
	Complete  bool
	EventBase model.Cursor
	Blobs     []ports.BlobKey
}

// WorkDepth counts one kind's stored work items: Pending items wait to be
// claimed, deferred ones included; Claimed items hold an unexpired claim;
// Quarantined items wait for RequeueWork. An item whose claim expired is
// pending again.
type WorkDepth struct {
	Pending     int
	Claimed     int
	Quarantined int
}

// SelectionKey names a candidate group: the conversations one selection
// decision chooses among, for example one identity's per-contact
// conversations in an inbox. Key encoding is engine-defined.
type SelectionKey struct {
	Inbox model.InboxRef
	Mode  string
	Key   string
}

// ConversationClaim claims a new conversation in a candidate group. CreateKey
// is derived only from the selection snapshot, so concurrent decisions over
// equal snapshots claim the same key and converge on one conversation.
type ConversationClaim struct {
	Selection    SelectionKey
	CreateKey    string
	Conversation model.Conversation
}

// MessageClaim claims a provider-assigned message identity for a new
// message: one message per inbox and Reference. The engine claims only
// references the provider assigns, such as a provider message ID unique
// per channel; a sender-chosen ID, such as an RFC 5322 Message-ID, is never
// claimed, since anyone can repeat it.
type MessageClaim struct {
	Reference model.ExternalRef
	Message   model.Message
}

// LinkRecord is the persisted identity link history record; the same value is
// read back through Reader.IdentityLinks.
type LinkRecord = model.IdentityLink

// LinkFilter selects link history by identity, by contact (as From or To), or
// both. At least one is required.
type LinkFilter struct {
	Identity *model.IdentityRef
	Contact  *model.ContactRef
}

// ProtocolState is adapter-owned state for one channel instance under Key.
// It holds protocol metadata and cursors, never domain entities or plaintext
// secrets.
type ProtocolState struct {
	Channel  model.ChannelRef
	Key      string
	Value    model.Extension
	Revision model.Revision
}

// RecipientLogKey addresses one recipient log: an adapter-owned, ordered
// log of what an adapter delivered to one recipient it hosts itself on one
// channel instance, such as a web widget visitor whose event stream resumes
// from a cursor. Namespace and Recipient are adapter-defined and required.
//
// A log of a recipient that is an identity must be addressed by the
// identity's own coordinates: Channel the channel of the identity's inbox,
// Namespace the identity's Namespace and Recipient its SourceID. Privacy
// erasure of a contact or identity (Privacy.Redact with Cascade) then
// deletes the log with Tx.EraseRecipientLog, transactionally with the rest
// of the erasure, and a log addressed otherwise is outside its reach. A
// recipient that is not an identity has no contact to erase.
type RecipientLogKey struct {
	Channel   model.ChannelRef
	Namespace string
	Recipient string
}

// Validate requires a valid channel reference, a namespace of at most 255
// bytes and a recipient of at most 1024 bytes, both UTF-8 without NUL.
func (k RecipientLogKey) Validate() error {
	if err := k.Channel.Validate(); err != nil {
		return err
	}
	if err := checkLogText("namespace", k.Namespace, 255); err != nil {
		return err
	}
	return checkLogText("recipient", k.Recipient, 1024)
}

func checkLogText(field, s string, limit int) error {
	switch {
	case s == "":
		return model.NewError(model.Validation, field+": required")
	case len(s) > limit:
		return model.NewError(model.Validation, field+": exceeds byte limit")
	case !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0:
		return model.NewError(model.Validation, field+": must be UTF-8 without NUL")
	}
	return nil
}

// Scope returns the owning tenant.
func (k RecipientLogKey) Scope() model.TenantID { return k.Channel.Tenant }

// RecipientLogEntry is one entry of a recipient log. Seq is its position in
// the log, assigned by AppendRecipientLog. Value is adapter-owned and opaque
// to the store; it holds what the adapter delivered, never secrets.
type RecipientLogEntry struct {
	Seq   uint64
	Value model.Extension
}

// IngressRecord is an accepted ingress claim. DedupKey is unique per channel.
type IngressRecord struct {
	ID              model.ID             `json:"id"`
	Channel         model.ChannelRef     `json:"channel"`
	VerifierVersion string               `json:"verifier_version"`
	DedupKey        string               `json:"dedup_key"`
	Input           model.Ingress        `json:"input"`
	Receipt         model.IngressReceipt `json:"receipt"`
}

// OperationKey is the operation identity: tenant, originating caller,
// operation kind, target and caller-supplied operation ID.
type OperationKey struct {
	Tenant model.TenantID    `json:"tenant"`
	Caller model.ActorID     `json:"caller"`
	Kind   string            `json:"kind"`
	Target model.Ref         `json:"target"`
	ID     model.OperationID `json:"id"`
}

// OperationRecord is the operation ledger entry: identity, versioned
// fingerprint, durable intent and current state. Operation.Pending is
// derived when an operation is read, never stored: writers leave it nil.
//
// Operation.UpdatedAt is the store's, never the writer's. Every read and
// the record ClaimOperation returns set it to the time of the operation's
// latest write (its claim, or its latest UpdateOperation), taken from the
// store clock and truncated to the microsecond, in UTC: the very value
// Reader.Operations orders by and bounds with Filter.Since and
// Filter.Before. A write ignores the UpdatedAt it is given, and a store
// does not keep it in the record: it comes from the write time it already
// holds. Redacting the originating message does not move it.
type OperationRecord struct {
	Operation          model.Operation `json:"operation"`
	Key                OperationKey    `json:"key"`
	FingerprintVersion uint32          `json:"fingerprint_version"`
	Fingerprint        []byte          `json:"fingerprint,omitempty"`
	Command            model.Extension `json:"command"`
	ExpiresAt          time.Time       `json:"expires_at"`
}

// Fence identifies one lease generation. Generations of a resource only grow.
type Fence struct {
	Owner      string `json:"owner"`
	Generation uint64 `json:"generation"`
}

// IsZero reports whether f is the zero fence.
func (f Fence) IsZero() bool { return f == Fence{} }

// AttemptEvidence is provider evidence for one attempt, appended under the
// fence the attempt ran with.
type AttemptEvidence struct {
	Operation          model.OperationRef  `json:"operation"`
	Attempt            model.ID            `json:"attempt"`
	Fence              Fence               `json:"fence"`
	State              model.DeliveryState `json:"state"`
	ProviderReferences []model.ExternalRef `json:"provider_references,omitempty"`
	ObservedAt         time.Time           `json:"observed_at"`
	Failure            *model.Error        `json:"failure,omitempty"`
	// ReadAt marks delivered evidence the provider also reported read, and
	// when (model.DeliveryReport.ReadAt).
	ReadAt *time.Time `json:"read_at,omitempty"`
}

// Work is a pending unit of background work of a registered kind, such as
// ingress processing, provider execution or event publication, on one
// resource. Fence, Until and Claims are set when the item is claimed. A
// non-zero NotBefore defers the item: it is not claimable before that time,
// judged by the store's clock. Claims counts the claims of the item since it
// was enqueued or requeued; a claim beyond the first means earlier owners
// did not finish it.
type Work struct {
	ID        model.ID
	Kind      string
	Resource  model.Ref
	NotBefore time.Time
	Fence     Fence
	Until     time.Time
	Claims    int
}

// WorkClaim asks for up to Limit items of Kinds, leased to Owner until Until.
// A positive MaxInFlight caps each kind's claimed items; see Tx.ClaimWork.
type WorkClaim struct {
	Kinds       []string
	Limit       int
	Owner       string
	Until       time.Time
	MaxInFlight int
}

// LeaseRequest asks for a lease on Resource for Owner until Until.
type LeaseRequest struct {
	Resource model.Ref
	Owner    string
	Until    time.Time
}

// Lease is a fenced, expiring claim on a resource. Expiry is judged by the
// store's clock.
type Lease struct {
	Resource model.Ref
	Fence    Fence
	Until    time.Time
}

// SubscriptionRecord is a persisted subscription: identity (principal,
// consumer, filter), accepted payload major, checkpoint and at most one
// outstanding batch. Its lease
// is the lease on the subscription's resource reference.
type SubscriptionRecord struct {
	Subscription model.Subscription `json:"subscription"`
	Principal    model.Principal    `json:"principal"`
	Consumer     string             `json:"consumer"`
	Filter       model.Filter       `json:"filter"`
	// PayloadMajor is the builtin payload major version the consumer
	// accepts.
	PayloadMajor uint32       `json:"payload_major"`
	PendingBatch *BatchRecord `json:"pending_batch,omitempty"`
	// ClosedAt is set once the subscription is closed; it keeps its
	// identity and position but issues no batches until reopened.
	ClosedAt *time.Time `json:"closed_at,omitempty"`
}

// BatchRecord binds an issued, unacknowledged batch to its scan range, event
// IDs, token digest, expiry and lease fence.
type BatchRecord struct {
	From        model.Cursor `json:"from"`
	Through     model.Cursor `json:"through"`
	EventIDs    []model.ID   `json:"event_ids,omitempty"`
	TokenDigest []byte       `json:"token_digest,omitempty"`
	ExpiresAt   time.Time    `json:"expires_at"`
	Fence       Fence        `json:"fence"`
}

// CheckpointAdvance moves a subscription checkpoint. Prior must equal the
// stored checkpoint and Fence the subscription's current, unexpired lease.
// When a batch is pending, Next must equal its Through and Fence its fence,
// and the batch must not have expired. Revision is the subscription's new
// revision.
type CheckpointAdvance struct {
	Subscription model.SubscriptionRef
	Prior        model.Cursor
	Next         model.Cursor
	Fence        Fence
	Revision     model.Revision
}

// AttachmentRecord binds public attachment metadata to its private blob or
// provider locator, staging operation, stager and lifecycle state. The
// locator never leaves the engine. Stager is the actor that created the
// record; Redact keeps it. State values are engine-defined, except
// AttachmentRedacted, which only Redact sets.
type AttachmentRecord struct {
	Attachment       model.Attachment   `json:"attachment"`
	Blob             *ports.BlobKey     `json:"blob,omitempty"`
	Provider         *model.ExternalRef `json:"provider,omitempty"`
	StagingOperation model.OperationRef `json:"staging_operation"`
	Stager           model.ActorID      `json:"stager,omitempty"`
	State            string             `json:"state"`
}

// AttachmentRedacted is the State of an attachment record Redact
// tombstoned: it holds no bytes and no locator.
const AttachmentRedacted = "redacted"

// ExportRecord binds a stored export to its private blob key, the
// operation that created it, the actor that asked for it and the manifest
// of the records it holds. The key and the manifest never leave the
// engine. A record without a manifest was written before manifests.
type ExportRecord struct {
	Export    model.Export       `json:"export"`
	Blob      ports.BlobKey      `json:"blob"`
	Operation model.OperationRef `json:"operation"`
	Creator   model.ActorID      `json:"creator"`
	Manifest  *ExportManifest    `json:"manifest,omitempty"`
}

// ExportManifest lists every record an export's bytes hold, so each open
// can reauthorize them against their current state.
type ExportManifest struct {
	Contacts      []model.ContactRef      `json:"contacts,omitempty"`
	Identities    []model.IdentityRef     `json:"identities,omitempty"`
	Conversations []model.ConversationRef `json:"conversations,omitempty"`
	Messages      []model.MessageRef      `json:"messages,omitempty"`
	Interactions  []model.InteractionRef  `json:"interactions,omitempty"`
	Attachments   []model.AttachmentRef   `json:"attachments,omitempty"`
	// Labels lists the label definitions written for the conversations'
	// label refs.
	Labels []model.LabelRef `json:"labels,omitempty"`
	// Tombstones lists the included messages, interactions and attachments
	// that were already redacted when the export was built; any other
	// included record redacted since supersedes the export.
	Tombstones []model.Ref `json:"tombstones,omitempty"`
}
