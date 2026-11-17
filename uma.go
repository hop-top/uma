// Package uma is an embeddable messaging engine: composition, an
// actor-bound client, lifecycle and typed application services.
//
// New validates a composition and returns an Engine; it performs no I/O and
// starts no goroutines. Every application service and runtime method has
// behaviour; an input a method does not support returns a *model.Error
// with code model.Unsupported, and its godoc says so. docs/api-coverage.md
// in the repository lists every method's status and store coverage.
package uma

import (
	"context"
	"io"
	"net/http"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Config is an engine composition. New copies its maps and slices; changing
// a Config after New has no effect on the engine. Every value is host-owned:
// the engine never closes one.
//
// Store, Authorizer, Clock and IDs are required. Everything else is optional:
// a missing policy denies the operations that need it, and a missing
// infrastructure port fails the operations that need it, never with an
// implicit global default.
type Config struct {
	// Store persists domain state. Required.
	Store store.Store
	// Authorizer decides every operation. Required; only an explicit allow
	// admits one.
	Authorizer policy.Authorizer
	// IdentityVerifiers maps an evidence kind (model.EvidenceKinds) to its
	// verifier; New rejects any other key. Evidence naming an unregistered
	// kind is unsupported. There is no default verifier: a host that links
	// on attestation alone registers model.EvidenceKindAttestation.
	IdentityVerifiers map[string]policy.IdentityVerifier
	// Selectors maps a conversation selection policy name to its selector.
	Selectors map[string]policy.ConversationSelector
	// InteractionValidator evaluates interaction submissions; nil denies them.
	InteractionValidator policy.InteractionValidator
	// CascadePolicy sets, per tenant and inbox, how many identities a
	// contact cascade's actor may not redact refuse it (see
	// policy.CascadePolicy). Nil gives every inbox
	// policy.CascadeNeverRefuse: denied identities are skipped and the
	// cascade is never refused for them.
	CascadePolicy policy.CascadePolicy
	// Adapters registers adapter factories by descriptor type key.
	Adapters []adapter.Factory
	// Extensions registers typed extension definitions.
	Extensions  []adapter.ExtensionDefinition
	Credentials ports.Credentials
	HTTP        ports.HTTPClient
	// Clock is the engine's time source. Required.
	Clock ports.Clock
	// IDs allocates internal resource IDs. Required.
	IDs      ports.IDs
	Blobs    ports.BlobStore
	Observer ports.Observer
	// Waiter paces Runtime.Run between claim rounds; nil uses a
	// cancellable stdlib timer.
	Waiter ports.Waiter
	// Retry bounds provider attempts and their backoff; zero fields take
	// defaults.
	Retry RetryPolicy
	// Retention sets each record family's horizon for Runtime.Purge. There
	// are no defaults: Purge fails while any horizon is zero; a negative
	// one is a composition error.
	Retention RetentionPolicy
	// Work bounds background work for Runtime.Run. Nil takes
	// DefaultMaxClaims and DefaultMaxInFlight; an explicit policy needs
	// both fields positive, or New rejects it.
	Work *WorkPolicy
	// RequireDurable rejects a Store whose Properties report Durable=false.
	RequireDurable bool
	// WebhookBodyLimit bounds, in bytes, the request body a handler from
	// Runtime.Webhook reads. A request declaring a longer body is answered
	// 413 before the adapter sees it; one that grows past the limit while
	// being read is answered 413 whatever the adapter writes. Zero takes
	// DefaultWebhookBodyLimit; negative is a composition error.
	WebhookBodyLimit int64
	// AttachmentSizeLimit bounds, in bytes, one attachment: Prepare rejects
	// a larger declared size and Upload stops reading past it. Zero takes
	// DefaultAttachmentSizeLimit; negative is a composition error.
	AttachmentSizeLimit int64
	// AttachmentMediaTypes lists the media types Prepare accepts, each a
	// type/subtype or type/* without parameters, matched case-insensitively
	// against the declared type without its parameters. Empty accepts any
	// well-formed media type; a malformed or duplicate entry is a
	// composition error.
	AttachmentMediaTypes []string
	// Held opts local writes that otherwise refuse a hold into being held
	// for a decision. The zero value holds none of them.
	Held HeldPolicy
}

// HeldPolicy lists the local writes a plugin actor's Command.Hold may
// hold beyond those always holdable. A hold on a write not opted in is
// model.Unsupported: nothing is claimed or applied. An opted-in write is
// stored as held, applied once when approved, and leaves nothing when
// rejected or expired.
type HeldPolicy struct {
	// LabelConfigure holds Labels.Configure, a label create or update. A
	// create reserves the label's ID when held and writes the label on
	// approval.
	LabelConfigure bool
	// LabelDelete holds Labels.Delete.
	LabelDelete bool
	// ConversationTransition holds Conversations.Transition.
	ConversationTransition bool
	// StartCreateAtHold sets when a held Conversations.Start, always
	// holdable, creates its conversation. False, the default: when the
	// start is approved, so a rejected or expired proposal leaves no
	// conversation and the inbox's selection policy chooses among the
	// conversations as they are then; the operation names its result
	// message only once approved. True: in the unit that holds the start,
	// which then names the conversation and message up front; a
	// conversation a rejected or expired start opened stays, empty.
	StartCreateAtHold bool
}

// DefaultWebhookBodyLimit is the webhook body limit when Config leaves
// WebhookBodyLimit zero: 1 MiB.
const DefaultWebhookBodyLimit int64 = 1 << 20

// DefaultAttachmentSizeLimit is the attachment size limit when Config
// leaves AttachmentSizeLimit zero: 25 MiB.
const DefaultAttachmentSizeLimit int64 = 25 << 20

// RetentionPolicy sets how long each record family is kept before
// Runtime.Purge deletes it, measured back from the engine clock at the
// purge. Every horizon is required to purge.
//
// Purging bounds the replay guarantees: an operation ID reused past the
// Operations horizon is a new command, a provider event redelivered past
// the Ingress horizon is accepted again, and a subscription checkpoint
// behind the purged Events prefix fails Poll with model.CursorExpired.
type RetentionPolicy struct {
	// Events is the event log horizon. The log is purged as a prefix: an
	// event stays while an older one does.
	Events time.Duration
	// Operations is the operation ledger horizon, from an operation's last
	// update. Operations with queued, claimed or quarantined work, or a live
	// lease, are kept.
	Operations time.Duration
	// Ingress is the provider event deduplication horizon, from receipt.
	Ingress time.Duration
	// Attempts is the attempt evidence horizon, for operations with no
	// work and no live lease.
	Attempts time.Duration
	// Staging is the attachment staging horizon, from the staging intent
	// an upload records before writing bytes. Purge deletes, from
	// Config.Blobs, the bytes of an intent older than it that no commit
	// bound, such as those a crash between staging and commit left
	// behind. It must exceed the longest upload: one still staging past it
	// fails at its commit with model.Conflict and keeps nothing.
	Staging time.Duration
	// Exports is the export artifact horizon, from an export's creation.
	// Purge deletes the export record and, from Config.Blobs, its bytes;
	// Privacy.Open of a purged export is model.NotFound.
	Exports time.Duration
	// Contacts is the orphaned contact horizon, from the latest link
	// record that moved an identity off the contact. Purge deletes a
	// contact no identity points at once that record is older than the
	// horizon; a contact an identity points at, or one no link ever left
	// such as a provisional contact, is kept whatever its age. A contact
	// merged away by Contacts.Merge, which leaves its redirect, stays
	// readable until then and is model.NotFound afterwards, redirect
	// included. Link history is never purged and keeps naming purged
	// contacts.
	Contacts time.Duration
}

// WorkPolicy bounds background work. Config.Work nil takes the defaults.
type WorkPolicy struct {
	// MaxClaims is how many claims of one work item may end unfinished, by
	// crash, lease expiry or execution error, before the next claim
	// quarantines it: its operation becomes model.DeadLettered and nothing
	// runs until Operations.Replay.
	MaxClaims int
	// MaxInFlight caps, per work kind and tenant, the items claimed at once
	// across every worker sharing the store. A claim round the cap leaves
	// empty is observed as ports.ObservationQueueSaturated and waits like
	// an idle one; work stays queued, never dropped.
	MaxInFlight int
}

// Work policy defaults, taken when Config.Work is nil.
const (
	// DefaultMaxClaims: ten unfinished claims ride out crashes and lease
	// expiries of a healthy fleet, yet stop a poison item after ten wasted
	// claims.
	DefaultMaxClaims = 10
	// DefaultMaxInFlight: sixteen claimed items per kind and tenant, sixteen
	// workers at the default WorkerOptions.Limit of one, bounds concurrent
	// provider calls without idling a small fleet.
	DefaultMaxInFlight = 16
)

// PurgeReport counts the records one Runtime.Purge deleted per family, and
// in Blobs the unbound staged blobs it deleted. EventBase is the earliest
// event cursor still accepted: subscriptions behind it fail with
// model.CursorExpired and reopen from beginning or tail.
type PurgeReport struct {
	Events     int
	Operations int
	Ingress    int
	Attempts   int
	Blobs      int
	Exports    int
	Contacts   int
	EventBase  model.Cursor
}

type Engine interface {
	For(model.Principal) (Client, error)
	Runtime() Runtime
	Describe() []adapter.Descriptor
	ValidateChannel(model.TypeKey, model.Extension) []model.ValidationIssue
	Close(context.Context) error
}
type Client interface {
	Configuration() Configuration
	Contacts() Contacts
	Conversations() Conversations
	Labels() Labels
	Messages() Messages
	WorkItems() WorkItems
	Tasks() Tasks
	Operations() Operations
	Events() Events
	Attachments() Attachments
	Extensions() Extensions
	Interactions() Interactions
	Streams() Streams
	Privacy() Privacy
}
type Configuration interface {
	// ConfigureChannel creates a channel (empty Ref.ID) or replaces one.
	// Operation key: kind channel.configure, target the channel ref; a
	// creation targets model.CreationTarget(tenant, model.KindChannel).
	ConfigureChannel(context.Context, model.ConfigureChannel) (model.Acceptance, error)
	// ConfigureInbox creates or replaces an inbox. Selection mode
	// model.ModePerAgentContext groups conversations by the agent context
	// reference of the ingress, never by sender, and takes no flag or rule
	// set. Operation key: kind inbox.configure, target the inbox ref; a creation
	// targets its channel ref.
	ConfigureInbox(context.Context, model.ConfigureInbox) (model.Acceptance, error)
	Channels(context.Context, model.ListRequest) (model.Page[model.Channel], error)
	Inboxes(context.Context, model.ListRequest) (model.Page[model.Inbox], error)
	// ChannelByKey and InboxByKey return the channel or inbox holding a
	// host-set key (Channel.Key, Inbox.Key), unique within the tenant.
	ChannelByKey(context.Context, model.LookupKey) (model.Channel, error)
	InboxByKey(context.Context, model.LookupKey) (model.Inbox, error)
	// Capabilities reports the current status of each capability the
	// channel's adapter declares, as an opened instance and its permission
	// probe report it. A declared capability the instance does not report
	// is unavailable (model.Unsupported), never assumed available; an
	// invalid report is model.Permanent. Availability is the declaration
	// intersected with engine support and current status: a capability no
	// operation of this version executes is unavailable (model.Unsupported)
	// whatever the adapter reports. Every builtin capability is executed
	// today: edit by Messages.Edit and edit-mode streams, delete by
	// Messages.Delete, react by Messages.React, work_item_act by
	// WorkItems.Act, respond and respond_follow_up by Interactions.Respond.
	// A channel whose adapter declares work_item_act lists it with the
	// instance's status; one that does not lists nothing.
	// Engine.Describe lists declarations only.
	Capabilities(context.Context, model.Lookup[model.ChannelRef]) ([]model.CapabilityStatus, error)
}

// Contacts reads contacts and identities and changes the contact an
// identity belongs to.
//
// An identity is one sender address in one inbox; it points at exactly one
// contact. A link is association only: it moves that pointer and, in the
// same commit, appends a link history record (model.IdentityLink) and an
// identity.linked event. It never moves, merges or closes conversations,
// messages, routes, work items or tasks, and never deletes a contact or
// identity. It changes no conversation selection: per-contact
// conversations stay keyed on the inbox and identity. It grants no access:
// every read stays authorized per inbox and audience. A reply uses its
// conversation's stored route, never another identity of the contact, and
// a message keeps the identity that authored it.
//
// Merging contact A into contact B is Merge: one explicit, authorized
// operation that moves every identity of A to B. It is never automatic
// and not undone by a Link back: A stays, with no identities and a
// redirect (model.Contact.MergedInto) to B, and a Link to a merged contact
// is model.Conflict. Each identity is moved like a relink, so Unlink
// detaches it again. Merge changes no conversation, message or route: the
// conversations an identity owns (model.Conversation.Owner) follow it to
// B because their owner is that identity. The redirect and the link
// history of both contacts are kept until a Runtime.Purge past
// RetentionPolicy.Contacts deletes A, and Contacts.History still lists
// every record after it.
//
// The engine classifies each change and authorizes it on the identity as
// one action:
//
//   - link: Link while the identity is still on the provisional contact
//     created with it; model.ActionIdentityLink, with the target contact
//     as destination.
//   - relink: Link from any other contact; model.ActionIdentityRelink.
//   - override: a link or relink whose verified assurance is below the
//     identity's current one also needs model.ActionIdentityLinkOverride.
//   - unlink: Unlink; model.ActionIdentityUnlink.
//   - no-op: Link to the identity's current contact;
//     model.ActionIdentityRead, nothing verified or recorded.
//
// Reads are authorized per record: model.ActionContactReadAll or
// model.ActionIdentityRead for List, model.ActionIdentityRead for
// Identities, model.ActionIdentityHistory for History.
//
// Evidence names its kind in Verifier; the verifier registered for that
// kind (Config.IdentityVerifiers) checks it outside any store unit, before
// any policy decision, and core checks what it returns
// (model.Evidence.ValidateReturn). An unregistered kind is
// model.Unsupported; evidence not verified is model.Validation and changes
// nothing. The verified level becomes the link assurance, ordered none <
// asserted < imported < attribute_match < host_authenticated < proven,
// and each kind is capped (model.MaxLinkAssurance). A (Verifier,
// Reference) pair backs one link only; reusing it is model.Conflict.
//
// Link and Unlink are committed local operations. ExpectedRevision is
// required and compared with the identity's revision, never a contact's; a
// stale one is model.Conflict. Success changes the identity's revision
// only. The acceptance's result is the identity ref. A repeated operation
// returns its stored result, authorized again but not verified again.
type Contacts interface {
	// List returns the contacts the principal may read: through
	// model.ActionContactReadAll on the contact, or through
	// model.ActionIdentityRead on at least one of its identities. A contact
	// with no identities is listed only through model.ActionContactReadAll.
	// Filter.Kinds narrows by contact kind; any other filter field is
	// model.Validation. Pages carry no total.
	List(context.Context, model.ListRequest) (model.Page[model.Contact], error)
	// Get returns one contact, admitted as List admits it, with the
	// revision Update expects. A contact the principal may not read is
	// model.Unauthorized; one of another tenant or gone is model.NotFound.
	Get(context.Context, model.Lookup[model.ContactRef]) (model.Contact, error)
	// Identities returns the identities the principal may read
	// (model.ActionIdentityRead); Filter.Contact selects one contact's and
	// Filter.Inboxes narrows by inbox; any other filter field is
	// model.Validation.
	// Identities in inboxes the principal may not read are left out, and
	// pages carry no total that would reveal them.
	Identities(context.Context, model.ListRequest) (model.Page[model.Identity], error)
	// Create records a contact no identity points at, for desk actors who
	// know a person before any channel does. Kind defaults to
	// model.ContactKindPerson; DisplayName is required and Attributes
	// (normalized, unverified, unique by kind and value, at most
	// model.MaxContactAttributes, stored sorted by kind then value) are
	// optional. It links no identity: the contact is unified with
	// identities by Link only, and until then List shows it only under
	// model.ActionContactReadAll. A committed operation authorized as
	// model.ActionContactWrite on model.CreationTarget(tenant,
	// model.KindContact), a different action from channel configuration;
	// the acceptance's result is the new contact ref. One
	// model.EventContactCreated for the internal audience commits with it.
	// Retrying with the same operation ID and payload returns the stored
	// operation; a different payload conflicts.
	// Operation key: kind contact.write, target
	// model.CreationTarget(tenant, model.KindContact).
	Create(context.Context, model.CreateContact) (model.Acceptance, error)
	// Update replaces a contact's display name and attributes, both whole,
	// at ExpectedRevision (required), authorized as
	// model.ActionContactWrite on the contact at that revision. The kind and
	// the contact's identities never change, and the new contact revision
	// is the acceptance's operation revision only; read the contact for it.
	// A stale revision is model.Conflict and changes nothing. When neither
	// field differs the call commits with the revision unchanged and no
	// event; otherwise one model.EventContactUpdated for the internal
	// audience names the fields that changed. A contact an identity claim
	// made may be updated like any other. A repeated operation returns its
	// stored result, authorized again, even once the revision has moved.
	// Operation key: kind contact.write, target the contact ref.
	Update(context.Context, model.UpdateContact) (model.Acceptance, error)
	// Link points an identity at an existing contact of the same kind. The
	// engine classifies it: link while the identity is on the provisional
	// contact created with it (model.ActionIdentityLink), relink otherwise
	// (model.ActionIdentityRelink), authorized on the identity with the
	// target contact as destination. Evidence is required; the verified
	// level becomes the link assurance, and a level below the identity's
	// current one also needs model.ActionIdentityLinkOverride. A target of
	// another kind is model.Validation, with no override: a bot or agent
	// identity never joins a person. A target equal to the current contact
	// is a no-op: committed without verification, record or event, the
	// revision unchanged, authorized as model.ActionIdentityRead.
	// Operation key: kind identity.link, target the identity ref, for link
	// and relink alike.
	Link(context.Context, model.LinkIdentity) (model.Acceptance, error)
	// Unlink detaches an identity to a fresh contact of the identity's kind
	// that the engine allocates, authorized as model.ActionIdentityUnlink on
	// the identity. Reason, an audit reason code, is required and recorded
	// on the link record. Evidence is optional: supplied, it is verified for
	// the unlink intent and reaches the authorizer as
	// policy.Authorization.Assurance and Evidence, so host policy can
	// require it; without it the authorizer sees no evidence and assurance
	// none. Evidence shows authority to detach, never that the identities
	// belong to different people. The link assurance becomes none.
	// Historical messages keep their author identity. The fresh contact is
	// read back through Identities or History.
	// Operation key: kind identity.unlink, target the identity ref.
	Unlink(context.Context, model.UnlinkIdentity) (model.Acceptance, error)
	// PreviewMerge lists what Merge of the contact From into the contact Into
	// would move, and changes nothing: the identities on From, the
	// conversations they own that the principal may read (found by
	// scanning the conversations of the identities' inboxes, so a tenant
	// with very many conversations in those inboxes gets a partial list,
	// flagged), and both contacts' revisions to pass to Merge. It is
	// authorized as model.ActionContactMerge on From and
	// model.ActionIdentityRead on every identity it lists; one denial
	// refuses the preview. A contact already merged is model.Conflict.
	PreviewMerge(context.Context, model.PreviewMerge) (model.MergePreview, error)
	// Merge merges the contact From into the contact Into, once: it moves
	// every identity on From to Into, each with a link history record of
	// class merge (who, when and Reason; link assurance asserted) and an
	// identity.linked event, sets From's redirect to Into, bumps both
	// contacts' revisions and appends one contact.merged event (refs only,
	// internal audience, resource Into). ExpectedRevision is From's
	// revision and ExpectedIntoRevision Into's, both from PreviewMerge, and
	// Identities must be exactly the identities previewed: if either
	// revision or the identities changed since, it is model.Conflict and
	// nothing moves. It is authorized as model.ActionContactMerge on From
	// with Into as destination, and for every identity as
	// model.ActionIdentityRelink at assurance asserted (plus
	// model.ActionIdentityLinkOverride where that is below the identity's
	// link assurance): a merge is no more permissive than relinking each
	// identity, and one denial refuses it all. A contact merged into
	// itself, one already merged, a kind mismatch and more than
	// model.MaxMergeIdentities identities are refused. Neither contact's
	// DisplayName or Attributes change. Two merges touching
	// one contact serialize: the second is model.Conflict. The result is
	// Into.
	// Operation key: kind contact.merge, target the From contact ref.
	Merge(context.Context, model.MergeContacts) (model.Acceptance, error)
	// History pages link history records in append order, selected by
	// Filter.Identity, Filter.Contact (as From or To) or both; neither is
	// model.Validation, as is any other filter field. Records are
	// authorized as model.ActionIdentityHistory on their identity; pages
	// carry no total.
	History(context.Context, model.ListRequest) (model.Page[model.IdentityLink], error)
}
type Conversations interface {
	// Get returns a conversation the principal may read
	// (model.ActionConversationRead). Its workflow state (Assignee, Snooze)
	// is zeroed unless the principal also holds
	// model.ActionConversationReadWorkflow on it, and Snooze is shown only
	// while in effect (not expired, not woken by a live inbound message;
	// an imported one, model.Message.ImportedBy, never wakes it).
	Get(context.Context, model.Lookup[model.ConversationRef]) (model.Conversation, error)
	// List pages the conversations the principal may read, in recording
	// order, each as Get shows it. It honours Filter.Inboxes, Statuses,
	// Labels and the workflow filters, evaluated by the store so pages are
	// full: Filter.Assignee (an actor, Self for the principal's actor, or
	// None for the unassigned) and Filter.Snoozed (true keeps the
	// conversations whose snooze is in effect, false the others). Under a
	// workflow filter only conversations whose workflow state the principal
	// may read (model.ActionConversationReadWorkflow) are listed.
	// Filter.Linked keeps the conversations holding a link the principal
	// may read (true) or none (false); Filter.Unread keeps those holding a
	// message unread for the principal (model.ReadMarker.Unread; its Actor,
	// when set, must be the principal's; model.Validation otherwise).
	// Imported history (Runtime.Import) neither wakes a snooze nor makes a
	// conversation unread.
	List(context.Context, model.ListRequest) (model.Page[model.Conversation], error)
	// Transition moves a conversation to another state.
	// Operation key: kind conversation.transition, target the conversation
	// ref.
	Transition(context.Context, model.Transition) (model.Acceptance, error)
	// Start sends the first message of an outbound-first conversation, to
	// a recipient no inbound message may have named, as a queued operation
	// like Messages.Reply whose result is the message. In one unit it
	// claims the recipient's identity as ingress would and lets the inbox's
	// selection policy pick or create the conversation
	// (policy.EventOutboundStart: built-in rules reuse an open or pending
	// one and never reopen); a created one routes to the recipient. The
	// adapter must declare model.CapabilityStartConversation; per-work-item
	// and per-agent-context inboxes are model.Unsupported. Authorized as
	// model.ActionConversationStart on the inbox, again before each
	// attempt. A retry with the same operation ID replays without selecting.
	// Operation key: kind conversation.start, target the inbox ref.
	Start(context.Context, model.StartConversation) (model.Acceptance, error)
	// AddLabels adds tenant labels to a conversation, authorized as
	// model.ActionConversationLabel on the conversation at its revision
	// (no model.ActionLabelRead on the labels). Every named label must
	// exist in the tenant, else model.NotFound and nothing changes. A label
	// the conversation already holds is a no-op; when all are, the call
	// commits with no event and the revision unchanged. Otherwise the
	// conversation revision moves once and one
	// model.EventConversationLabelAdded is emitted per label added.
	// ExpectedRevision is optional; when set, a stale one is
	// model.Conflict. A conversation holds at most
	// model.MaxConversationLabels labels; growing past it is
	// model.Conflict. Retrying with the same operation ID and payload
	// returns the stored operation; a different payload, or the same
	// operation ID on RemoveLabels, conflicts.
	// Operation key: kind conversation.label, target the conversation ref.
	AddLabels(context.Context, model.ConversationLabels) (model.Acceptance, error)
	// RemoveLabels removes labels from a conversation, as AddLabels: a
	// label the conversation does not hold is a no-op, and one
	// model.EventConversationLabelRemoved is emitted per label removed.
	// Operation key: kind conversation.label, target the conversation ref.
	RemoveLabels(context.Context, model.ConversationLabels) (model.Acceptance, error)
	// Capabilities reports the current status of each capability the
	// adapter of the conversation's channel declares, exactly as
	// Configuration.Capabilities reports it for that channel (the inbox's
	// channel), so a desk client can tell whether a verb will work on the
	// conversation before trying it, a capability the channel
	// configuration disables included (reason model.Disabled). It returns
	// statuses only, no channel configuration. Authorized as
	// model.ActionConversationRead on the conversation at its revision
	// under the internal audience (model.Internal), never as
	// model.ActionChannelRead: a policy that limits a participant to public
	// content refuses it. A refused read opens no adapter instance; an
	// admitted one opens an instance and may perform provider I/O.
	Capabilities(context.Context, model.Lookup[model.ConversationRef]) ([]model.CapabilityStatus, error)
	// Assign sets the conversation's assignee (model.Conversation.Assignee)
	// to req.Assignee, required, replacing any: local desk workflow state,
	// never a provider effect. Authorized as model.ActionConversationAssign
	// on the conversation at its revision; the assignee must be eligible:
	// the authorizer is asked whether the assignee itself may perform
	// model.ActionConversationAssign on the conversation, and a denial is
	// model.Unauthorized. Assigning the current assignee commits with no
	// event and the revision unchanged; otherwise the revision moves once
	// and one model.EventConversationAssigned is emitted (internal
	// audience, references only: assignee and previous assignee), so the
	// event log keeps the assignment history. ExpectedRevision is optional;
	// when set, a stale one is model.Conflict. Retrying with the same
	// operation ID and payload returns the stored operation; a different
	// payload conflicts.
	// Operation key: kind conversation.assign, target the conversation ref.
	Assign(context.Context, model.ConversationAssignment) (model.Acceptance, error)
	// Unassign clears the assignee, as Assign with req.Assignee empty and
	// no eligibility question; an unassigned conversation is a no-op, and
	// a change emits model.EventConversationUnassigned.
	// Operation key: kind conversation.assign, target the conversation ref.
	Unassign(context.Context, model.ConversationAssignment) (model.Acceptance, error)
	// Snooze snoozes the conversation until req.Until, which must be after
	// the engine clock's now (model.Validation otherwise), recording that
	// now as Snooze.At and the caller as Snooze.By and replacing any
	// snooze. The snooze hides the conversation from lists that exclude
	// snoozed ones (Filter.Snoozed) until Until or until a live inbound
	// message is recorded after At, whichever comes first (a message
	// Runtime.Import records is no new inbound): evaluated when read, so
	// nothing is scheduled, it holds across restarts, and waking emits no
	// event. It is independent of Status. Authorized as
	// model.ActionConversationSnooze on the conversation at its revision;
	// the revision moves once and one model.EventConversationSnoozed is
	// emitted (internal audience, references and Until). ExpectedRevision
	// and replay as for Assign.
	// Operation key: kind conversation.snooze, target the conversation ref.
	Snooze(context.Context, model.ConversationSnooze) (model.Acceptance, error)
	// Unsnooze ends the snooze early, as Snooze with req.Until unset; a
	// conversation whose snooze is not in effect (none, expired or woken)
	// is a no-op, and a change emits model.EventConversationUnsnoozed.
	// Operation key: kind conversation.snooze, target the conversation ref.
	Unsnooze(context.Context, model.ConversationSnooze) (model.Acceptance, error)
	// Link links the conversation to req.Link, another conversation of
	// the tenant or a work item: association only, with no history merge,
	// no reply-scope change and no provider effect. Authorized on both
	// ends: model.ActionConversationLink on the conversation at its
	// revision, and on a far conversation at its revision, or
	// model.ActionWorkItemRead on a far work item. A link between two
	// conversations is held by both (model.Conversation.Links), written in
	// one unit; a link to a work item by the conversation alone. A link
	// already held commits with no event; otherwise each conversation end
	// moves its revision once and emits one model.EventConversationLinked
	// naming its far end (internal audience, references only, delivered
	// only to subscribers that may read that far end). A conversation
	// holds at most model.MaxConversationLinks links; growing past it is
	// model.Conflict. ExpectedRevision (of the conversation) and replay as
	// for Assign. Readers see only the links whose far end they may read.
	// Operation key: kind conversation.link, target the conversation ref.
	Link(context.Context, model.ConversationLinkChange) (model.Acceptance, error)
	// Unlink removes a link, as Link: an absent link is a no-op, and each
	// end changed emits model.EventConversationUnlinked.
	// Operation key: kind conversation.link, target the conversation ref.
	Unlink(context.Context, model.ConversationLinkChange) (model.Acceptance, error)
	// MarkRead moves the caller's own read marker (model.ReadMarker) so
	// that req.Message, a message of the conversation, or every message
	// recorded until now when unset, is read. Authorized as
	// model.ActionConversationMarkRead on the conversation, and
	// model.ActionMessageRead on req.Message. The marker is a record of
	// its own: the conversation's revision never moves and no event is
	// emitted. Setting the position the marker holds changes nothing, so
	// a retry after a lost answer is safe; it takes no operation ID.
	MarkRead(context.Context, model.ReadMark) (model.ReadMarker, error)
	// MarkUnread moves the caller's read marker so that req.Message, or
	// the newest live inbound message not imported by Runtime.Import when
	// unset (every message when there is none), and everything recorded
	// after it is unread; otherwise as MarkRead. Imported messages never
	// count as unread, so a conversation holding only those after the
	// position stays read.
	MarkUnread(context.Context, model.ReadMark) (model.ReadMarker, error)
	// ReadMarker returns the caller's read marker on the conversation, one
	// with a zero ReadThrough and no revision when it has none. Authorized
	// as model.ActionConversationReadWorkflow; no call reads another
	// actor's marker.
	ReadMarker(context.Context, model.Lookup[model.ConversationRef]) (model.ReadMarker, error)
	// AddParticipant queues adding an identity of the conversation's inbox
	// to the provider conversation, so helpdesk-style clients can bring
	// someone in. The channel's adapter must declare
	// model.CapabilityMembership, else model.Unsupported, and the identity
	// must be stored, else model.NotFound, both checked before anything is
	// stored. Authorized as model.ActionConversationMembership on the
	// conversation (internal audience), again before each attempt.
	// ExpectedRevision is optional; when set, a stale one is
	// model.Conflict, as is adding an identity that already is a
	// participant.
	//
	// It returns once the operation and its work commit, as model.Queued,
	// with the conversation as Result; Runtime.Run executes it. The
	// conversation changes only once the provider applied it: the identity
	// joins Conversation.Participants (role "participant"), the revision
	// moves and conversation.changed reports participants. A rejected or
	// failed change leaves the conversation as it is. A lost answer is
	// reconciled, never repeated blindly. Retrying with the same operation
	// ID and payload returns the stored operation; a different payload, or
	// the same operation ID on RemoveParticipant, conflicts.
	// Operation key: kind conversation.membership, target the conversation
	// ref.
	AddParticipant(context.Context, model.ParticipantChange) (model.Acceptance, error)
	// RemoveParticipant queues removing a participant from the provider
	// conversation, with AddParticipant's rules: removing an identity that
	// is no participant, or the conversation's owner, is model.Conflict;
	// once applied the identity leaves Conversation.Participants. A later
	// inbound message from it adds it back, as for any sender.
	// Operation key: kind conversation.membership, target the conversation
	// ref.
	RemoveParticipant(context.Context, model.ParticipantChange) (model.Acceptance, error)
	// Archive queues archiving the conversation at the provider, or
	// unarchiving it when Undo is set; Move, SetRead, Flag and Trash do
	// the same for moving it, marking it read or unread, flagging or
	// unflagging it, and trashing or restoring it. Each is carried by one
	// capability (model.CapabilityArchive, CapabilityMove,
	// CapabilityReadState, CapabilityFlag, CapabilityTrash) the channel's
	// adapter must declare, else model.Unsupported, and report available
	// with its limits (model.StateLimits): a capability that is not
	// available, or whose limits are missing or unreadable, is refused
	// the same way, all before anything is stored. Authorized as
	// model.ActionConversationStateAccount or
	// model.ActionConversationStateShared on the conversation, whichever
	// the scope the adapter reported for the verb names, again before each
	// attempt; the caller must also be allowed to read the conversation
	// before any provider is asked for its limits.
	// Move needs a destination or label changes the reported shape accepts
	// (folder: a destination only; labels: either) and, for a role, one
	// the adapter resolves; Flag needs a kind the adapter reported.
	// ExpectedRevision is optional; when set, a stale one is
	// model.Conflict.
	//
	// They return once the operation and its work commit, as
	// model.Queued, with the conversation as Result; Runtime.Run executes
	// it. The conversation's provider state (Conversation.ProviderState)
	// changes only once the provider applied it, and not when provider
	// state of the same part advanced since the call: the newer inbound
	// state wins and the operation keeps the provider's answer. Until
	// then Operations.Get shows the change as Operation.PendingState. A
	// rejected or failed change leaves the state as it is; a lost answer
	// is reconciled, never repeated blindly. Local status and labels are
	// never touched. Retrying with the same operation ID and payload
	// returns the stored operation; a different payload conflicts.
	// Operation key: kind conversation.state_account or
	// conversation.state_shared, target the conversation ref.
	Archive(context.Context, model.ArchiveConversation) (model.Acceptance, error)
	// Move moves the conversation at the provider: see Archive.
	// Operation key: kind conversation.state_account or
	// conversation.state_shared, target the conversation ref.
	Move(context.Context, model.MoveConversation) (model.Acceptance, error)
	// SetRead marks the conversation read or unread at the provider; it
	// never changes the per-actor read marker (MarkRead, MarkUnread). See
	// Archive.
	// Operation key: kind conversation.state_account or
	// conversation.state_shared, target the conversation ref.
	SetRead(context.Context, model.SetConversationRead) (model.Acceptance, error)
	// Flag flags or unflags the conversation at the provider: see Archive.
	// Operation key: kind conversation.state_account or
	// conversation.state_shared, target the conversation ref.
	Flag(context.Context, model.FlagConversation) (model.Acceptance, error)
	// Trash moves the account's copy to the provider's trash, or restores
	// it; it is not Messages.Delete. See Archive.
	// Operation key: kind conversation.state_account or
	// conversation.state_shared, target the conversation ref.
	Trash(context.Context, model.TrashConversation) (model.Acceptance, error)
}

// Labels manages the tenant's label definitions (model.Label): tenant
// vocabulary reusable across every inbox. Conversations hold labels by
// reference (model.Conversation.Labels) and change them through
// Conversations.AddLabels and RemoveLabels. Definitions emit no events.
type Labels interface {
	// Configure creates a label (empty Ref.ID) or replaces one at
	// ExpectedRevision, as a committed operation authorized as
	// model.ActionLabelConfigure. A name equal to another label's under
	// simple case folding (model.LabelNameKey) is model.Conflict.
	// Renaming keeps every assignment. A creation is authorized on
	// model.CreationTarget(tenant, model.KindLabel), a replacement on the
	// label at ExpectedRevision. Retrying with the same operation ID and
	// payload returns the stored operation; a different payload conflicts.
	// Operation key: kind label.configure, target the label ref; a
	// creation targets model.CreationTarget(tenant, model.KindLabel).
	Configure(context.Context, model.ConfigureLabel) (model.Acceptance, error)
	// Get returns a label the principal may read
	// (model.ActionLabelRead).
	Get(context.Context, model.Lookup[model.LabelRef]) (model.Label, error)
	// List pages the tenant's labels the principal may read, in recording
	// order; labels it may not read are left out. No filter field is
	// honoured: any set field is model.Validation.
	List(context.Context, model.ListRequest) (model.Page[model.Label], error)
	// ByName returns the label whose name equals Key under simple case
	// folding (model.LabelNameKey), authorized as model.ActionLabelRead.
	ByName(context.Context, model.LookupKey) (model.Label, error)
	// Delete deletes a label definition at ExpectedRevision, authorized as
	// model.ActionLabelConfigure. A label any conversation still holds is
	// model.Conflict: remove it from those conversations first. A retry
	// with the same operation ID replays, even once the label is gone.
	// Operation key: kind label.configure, target the label ref.
	Delete(context.Context, model.DeleteLabel) (model.Acceptance, error)
}
type Messages interface {
	Get(context.Context, model.Lookup[model.MessageRef]) (model.Message, error)
	// Raw reads the provider's whole copy of a message on demand: for mail
	// the RFC 5322 bytes with every header and part. Authorized like Get
	// (model.ActionMessageRead under the message's audience), and the
	// channel's adapter must declare model.CapabilityRawRead, else
	// model.Unsupported. Nothing is stored: the bytes are read from the
	// provider at each call and handed to the caller alone. A message over
	// the adapter's size bound is model.Permanent with the reason
	// model.TooLarge, never a truncated copy. A redacted or deleted message
	// is model.Conflict with no provider call: its content was scrubbed, and
	// the provider's copy is not served around that. A message with no
	// provider message reference (an internal note, a message not yet
	// delivered) is model.Conflict too. The provider no longer holding the
	// message is model.NotFound.
	Raw(context.Context, model.Lookup[model.MessageRef]) (model.RawMessage, error)
	// List pages the messages the principal may read
	// (model.ActionMessageRead under each message's audience) in recording
	// order, narrowed by Filter.Inboxes, Filter.Conversation, Filter.Task
	// (the task's message history: the messages bound to it, Message.Task),
	// Filter.Labels (the labels the message's conversation holds when the
	// page is read; also needs model.ActionConversationRead on that
	// conversation, or the message is skipped) and the message search fields Filter.Text, Since,
	// Before and Attachments (rules with model.MaxSearchTextBytes); any
	// other filter field is model.Validation. Search changes neither order
	// nor admission: a message the principal may not read is skipped
	// without trace, whatever it matches, and pages carry no total. With
	// Attachments.MediaTypes set, a message is listed only when
	// model.ActionAttachmentRead allows a matching attachment under both
	// the attachment's audience and the message's. A search page that
	// completes the query carries no Next: rerun the search to find
	// messages recorded since. One call scans at most 500 stored matches,
	// readable or not, so a page can be short or empty and still be
	// incomplete; loop until Complete. A list that is not a search does not
	// seal its completing page, and an empty page with a Next tells the
	// caller that records it may not read exist; the README section
	// "Searching messages" states the rules and this accepted limit.
	List(context.Context, model.ListRequest) (model.Page[model.Message], error)
	// Reply sends a public reply in a conversation. The reply records the
	// message it answers (Message.InReplyTo); when that message is bound
	// to a task, the reply joins the task's history (Message.Task). A reply
	// naming InResponseTo is correlated with that interaction, which must
	// exist in the conversation and be readable by the caller
	// (model.NotFound otherwise): the message and its message.created event
	// record it, and the reply never claims the interaction's initial
	// response, counts a follow-up or reads its response token.
	// Recipients, when it names any, addresses the reply beyond the
	// conversation's reply route (reply to all, Cc, Bcc): the channel's
	// adapter must declare model.CapabilityReplyRecipients, else
	// model.Unsupported, and the status it reports carries what it
	// supports (model.RecipientLimits). The addresses are normalized and
	// are part of the operation's fingerprint; a held reply keeps them.
	// Operation key: kind message.reply, target the conversation ref.
	Reply(context.Context, model.Reply) (model.Acceptance, error)
	// AddInternalNote adds an internal note to a conversation.
	// Operation key: kind message.note, target the conversation ref.
	AddInternalNote(context.Context, model.InternalNote) (model.Acceptance, error)
	// Edit queues a provider edit of a message's content as an operation
	// whose Result and ResultMessage are the message. The channel's adapter
	// must declare model.CapabilityEdit, else model.Unsupported. Authorized
	// as model.ActionMessageEdit on the message under its audience at
	// ExpectedRevision; no uma origin is required, the provider account
	// decides which messages it may edit. ExpectedRevision is required: a
	// message at another revision, deleted, redacted or a live stream's is
	// model.Conflict. It returns once the operation and its work commit, as
	// model.Queued; the message is untouched until the provider applied the
	// edit, and Operations.Get shows the change as Operation.Pending until
	// then. Messages.Get and List never carry pending content. An edit the
	// provider applied after a newer inbound edit of the message keeps the
	// newer content: the operation settles with the provider's answer and
	// message.updated reports edit_superseded.
	// Retrying with the same operation ID and payload returns the stored
	// operation; a different payload conflicts. The payload includes the
	// message's audience at the time of the call: a narrowed audience is a
	// different command, so a retry after the audience narrowed is
	// model.Conflict, not a replay. Operations.Lookup still finds the
	// stored operation by its key.
	// Operation key: kind message.edit, target the message ref.
	Edit(context.Context, model.EditMessage) (model.Acceptance, error)
	// Delete queues a provider deletion of a message, with Edit's rules
	// under model.CapabilityDelete and model.ActionMessageDelete. Once
	// applied the message is a tombstone (Message.Deleted). It is no
	// privacy redaction: see Privacy.Redact.
	// Operation key: kind message.delete, target the message ref.
	Delete(context.Context, model.DeleteMessage) (model.Acceptance, error)
	// React queues adding the caller's reaction to a message, or removing
	// it, with Edit's rules under model.CapabilityReact and
	// model.ActionMessageReact, except that ExpectedRevision is optional
	// and compared only when set.
	// Operation key: kind message.react, target the message ref.
	React(context.Context, model.Reaction) (model.Acceptance, error)
	// Receipt queues a delivery or read receipt (model.ReceiptDelivered,
	// model.ReceiptRead) for a received message, so its sender sees that
	// it arrived or was read. The channel's adapter must declare
	// model.CapabilityReceipt, else model.Unsupported, checked before
	// anything is stored. Authorized as model.ActionMessageReceipt on the
	// message under its audience. The message must have arrived from the
	// provider: one sent through uma, or the provider's echo of one, is
	// model.Validation; a deleted or redacted one is model.Conflict.
	// ExpectedRevision must be empty.
	//
	// It returns once the operation and its work commit, as model.Queued;
	// Runtime.Run sends it, and the operation is the record that it was
	// sent: nothing else in uma changes. Retrying with the same operation
	// ID and kind returns the stored operation and sends nothing again; a
	// different kind under the same ID is model.Conflict. An attempt whose
	// answer was lost is reconciled, never sent again blindly.
	// Operation key: kind message.receipt, target the message ref.
	Receipt(context.Context, model.Receipt) (model.Acceptance, error)
}
type WorkItems interface {
	Get(context.Context, model.Lookup[model.WorkItemRef]) (model.WorkItem, error)
	List(context.Context, model.ListRequest) (model.Page[model.WorkItem], error)
	// Act changes a work item through a model.SchemaWorkItemUpdate action.
	// Field ownership routes it: fields the work item declares local
	// (WorkItem.LocalFields) commit in uma as a local operation; a status
	// the provider owns, with an optional model.WorkItemUpdate.StatusReason,
	// is an outbound operation (Queued) the provider settles, along a
	// transition the channel adapter declares (model.CapabilityWorkItemAct,
	// adapter.Descriptor.WorkItemActs; an undeclared transition or reason is
	// model.Unsupported). The work item changes only once the provider
	// succeeded; its own webhook of the change never settles the operation.
	// When the provider's success returns the work item's state, that state
	// is applied in source order; when it returns none (and when a lost
	// answer is settled by reconciliation), the work item stays as it is
	// until the provider's own webhook arrives. Settling queues no refetch,
	// since no principal exists then to run one.
	// A provider-owned status with any other field, or a status reason with
	// a local status, is model.Validation. Other provider-owned fields are
	// ingest-only: model.Unsupported. A local closed status is terminal; a
	// provider-owned one reopens where the adapter declares it. Act never
	// changes the conversation's status.
	//
	// Operation key: kind work_item.act, target the work item ref.
	Act(context.Context, model.WorkItemAction) (model.Acceptance, error)
}

// Tasks reads and acts on agent protocol tasks: work the host executes for
// a remote agent in a per-agent-context conversation, many per
// conversation, each with its own lifecycle (model.Task). The host opens
// tasks and reports their progress; uma never changes a task's state by
// itself. Callers are host actors under the host authorizer. A remote agent
// reaches its tasks through an agent protocol adapter, such as the
// a2a.messages adapter (docs/adapters/a2a.messages.md), never through this
// service.
type Tasks interface {
	// Get returns a task the principal may read (model.ActionTaskRead). A
	// reference in another tenant, or naming the task under another
	// conversation, is model.NotFound.
	Get(context.Context, model.Lookup[model.TaskRef]) (model.Task, error)
	// List pages the tasks the principal may read, most recent status
	// first (StatusAt, then ID, descending), narrowed by Filter.Inboxes,
	// Filter.Conversation and Filter.Statuses (task states); any other
	// filter field, Identity included, is model.Validation. Each task is
	// admitted under model.ActionTaskRead; others are skipped without
	// trace.
	List(context.Context, model.ListRequest) (model.Page[model.Task], error)
	// Act applies a builtin task action as a committed operation whose
	// Result and ResultTask are the task, authorized as
	// model.ActionTaskAct. Role or author fields in the action are
	// model.Validation: the actor is always the caller.
	//
	// model.SchemaTaskOpen (no ExpectedRevision; Target names the
	// conversation, its ID empty) opens a task for a message of the
	// conversation, authorized on the conversation. Only a
	// per-agent-context conversation holds tasks: any other is
	// model.Validation. A message already bound to a task is
	// model.Conflict. The task starts in the given state, submitted by
	// default; the message is bound to it (Message.Task) and task.updated
	// (created) is recorded in the same unit.
	//
	// model.SchemaTaskUpdate (ExpectedRevision required) changes a task,
	// authorized on the task. A move model.CanTransition refuses, any
	// update of a terminal task, or another revision is model.Conflict;
	// interrupted states resume to working. Artifacts upsert by ID;
	// reaching canceled clears CancelRequested. A state change without
	// status content records task.updated only. Status content records
	// exactly one uma-authored message bound to the task (origin
	// model.MessageOriginTaskStatus, under the action's audience or else a
	// public one), sets Task.StatusMessage, and emits message.created and
	// task.updated naming it; it is never sent through the channel: the
	// remote agent reads it through the task. An update that changes
	// nothing commits without event.
	//
	// Retrying with the same operation ID and payload returns the stored
	// operation even after the task moved on; a different payload is
	// model.Conflict.
	//
	// Operation key: kind task.act, target the task ref; opening: the
	// conversation ref.
	Act(context.Context, model.TaskAction) (model.Acceptance, error)
}
type Operations interface {
	// Get returns an operation's current state. An operation waiting for
	// its next attempt after a failure that applied nothing is
	// model.RetryPending, with that failure and its retry advice; queued
	// is only an operation no attempt has answered yet. Operation.UpdatedAt
	// is when it was last written, the time Operations.List orders and
	// bounds by; Lookup, List and Reconcile show it the same way.
	// Operation.Key is the key its command claimed (caller, kind, target,
	// operation ID): every acceptance and every read carries it, so a
	// listed operation shows the Kind, Target and ID that Lookup takes.
	Get(context.Context, model.Lookup[model.OperationRef]) (model.Operation, error)
	// List pages the operations the principal may read, most recently
	// written first: the history of what this tenant's commands became.
	// "Written" is the time of an operation's latest change in the store: it
	// is accepted, then moves at every state change (an attempt, a retry
	// scheduled, a settling, a replay). It honours
	//
	//   - Filter.Inboxes: operations whose target lies in one of the
	//     inboxes (an inbox's own configuration included);
	//   - Filter.Conversation: operations on that conversation or producing
	//     a message, stream or task in it (Messages.Reply, Edit, Delete,
	//     React, AddInternalNote, Conversations.Transition, Start,
	//     AddLabels, RemoveLabels, Streams.Start, Update, End, Tasks.Act,
	//     Interactions.Ephemeral), but not an operation on a work item, an
	//     interaction or an attachment, which record no conversation;
	//   - Filter.Statuses: model.DeliveryState values, any of them; an
	//     unknown state is model.Validation;
	//   - Filter.Since (inclusive) and Filter.Before (exclusive): the window
	//     on the time written, to the microsecond; Before must be later
	//     than Since;
	//   - Filter.Kinds: operation kinds, any of them (Operation.Key.Kind,
	//     the kind on each mutating method's "Operation key" line); a kind
	//     no operation has selects none;
	//   - Filter.Actor: the operations one actor sent
	//     (Operation.Key.Caller), such as the principal's own.
	//
	// The filters combine (all must hold). Any other filter field is
	// model.Validation, as is a malformed cursor, and a reference in the
	// filter that another tenant owns is model.NotFound. The page size is
	// that of every list: 50 by default, at most 100.
	//
	// Each operation is authorized as Get authorizes it
	// (model.ActionOperationRead at the operation's revision) and is shown
	// as Get shows it, Pending, Key and UpdatedAt included. The list
	// therefore shows every operation the principal may read, whoever sent
	// it; Filter.Actor narrows it to one sender and never widens what
	// model.ActionOperationRead allows. A held operation (state held, Held
	// set) is also shown only to a principal that may read its target, and
	// is left out otherwise; Filter.Statuses "held" lists the held ones.
	// Kinds and Actor are matched in the
	// engine on each operation's key, not by the store, so a page they
	// thin may be short. UpdatedAt is the time
	// the list orders by, newest first, and the one Filter.Since and
	// Filter.Before compare, so an operation listed in a window shows a
	// time inside it. One the principal may not read is
	// left out without trace: no total, no placeholder. A page may be short
	// or empty and still carry Next; Complete reports that no further
	// operation was found. The cursor names the position of the last
	// operation scanned, never an offset, so operations accepted after a
	// page was read never shift the next one; it is bound to the caller,
	// the tenant and the filter, and a cursor altered or replayed by
	// another caller or for another filter is model.Validation. An
	// operation written again after a page was read moves to the front,
	// where that listing will not return to it: read a listing once, and
	// start again to see changes. The list is bounded by the retention
	// horizon (RetentionPolicy.Operations): purged operations are gone.
	List(context.Context, model.ListRequest) (model.Page[model.Operation], error)
	// Replay queues a model.DeadLettered operation again, authorized for
	// model.ActionOperationReplay as the current principal; execution then
	// reauthorizes the originating actor. Any other state is
	// model.Conflict.
	// Operation key: kind operation.replay, target the replayed
	// operation's ref.
	Replay(context.Context, model.Lookup[model.OperationRef], model.Command) (model.Acceptance, error)
	// Approve, Reject and Edit decide a held operation (model.Held; see
	// model.HeldBlock). The decider is the current principal: it needs the
	// read right of a held operation (model.ActionOperationRead and the
	// target's read right, else model.NotFound) and then the decision's own
	// action, model.ActionOperationApprove, model.ActionOperationReject or
	// model.ActionOperationEdit; a denial is model.Unauthorized. Proposing
	// grants none of them. Held.DecidedBy is that principal's actor.
	//
	// Each decision is compare-and-swap on the operation's revision
	// (model.DecideHeld.ExpectedRevision, required; a mismatch is
	// model.Conflict) and is refused as model.Conflict when the target, a
	// conversation, advanced since the hold, unless DecideHeld.Force is set.
	// A held operation past its deadline cannot be approved or edited. An
	// operation that is not held is model.NotFound.
	//
	// Approve moves the operation to queued and applies it under the
	// operation key its proposal claimed: the same operation, the same
	// references it named when held. Edit does the same with the edited
	// arguments (model.EditHeld.Args, the intent's name and version, new
	// data), recording before and after in Held.Edits; Held.Decision is
	// edited. Reject moves it to rejected: nothing is applied and no work is
	// queued. Held.DecisionNote records the note.
	//
	// A decision already made is returned as the operation stands, with
	// model.Decided.Replayed set and nothing written: a second approve, a
	// second reject or an edit repeated with the same arguments. Any other
	// decision of a decided operation is model.Conflict. A replay of the
	// original proposal after an edit still returns the operation: the
	// stored fingerprint is the proposal's.
	//
	// The result carries the trust key, its labels and whether the decision
	// is a correction (an edit or a reject), which is what a host counts
	// trust on; a replayed result must not be counted again. uma counts
	// each decision under the trust key itself, in the decision's unit: an
	// approval as proposed is one clean approval, or, when
	// model.DecideHeld.Batch is set, one for all approvals of that key
	// sharing the batch; a reject or an edit is a correction; an expiry
	// moves nothing.
	// Operation key: none; the decisions claim no key of their own.
	Approve(context.Context, model.DecideHeld) (model.Decided, error)
	Reject(context.Context, model.DecideHeld) (model.Decided, error)
	Edit(context.Context, model.EditHeld) (model.Decided, error)
	// Trust reads the counters a decision moves under one host-set trust key
	// (model.TrustEntry; see the held-operations decision record):
	// consecutive clean approvals and totals. uma counts; the host sets any
	// threshold. Authorized for the trust.read action. Trust is per channel
	// of the operations' target inbox.
	//
	// The request names the channel (TrustLookup.Channel and the others).
	// Operation key: none.
	Trust(context.Context, model.TrustLookup) (model.TrustEntry, error)
	// Trusts lists trust entries, filtered by label, as Trust reads one.
	//
	// The request names the channel (TrustLookup.Channel and the others).
	// Operation key: none.
	Trusts(context.Context, model.TrustFilter) (model.Page[model.TrustEntry], error)
	// ResetTrust zeroes the clean count of one trust key and keeps its
	// totals. Authorized for the trust.reset action.
	//
	// The request names the channel (TrustLookup.Channel and the others).
	// Operation key: none.
	ResetTrust(context.Context, model.ResetTrust) (model.TrustEntry, error)
	// ResetTrustSet resets several trust keys, all or none in one unit.
	//
	// The request names the channel (TrustLookup.Channel and the others).
	// Operation key: none.
	ResetTrustSet(context.Context, model.ResetTrustSet) ([]model.TrustEntry, error)
	// ImportTrust seeds the counters of one trust key from an earlier approval
	// history, idempotently: totals become the larger of stored and imported,
	// LastCorrectionAt the later, and the consecutive count is raised only
	// while no stored correction is later than the imported one. A repeat, or
	// an import lower than live counts, changes nothing. Returns the entry
	// as stored. Authorized for the trust.write action.
	//
	// The request names the channel (TrustLookup.Channel and the others).
	// Operation key: none.
	ImportTrust(context.Context, model.ImportTrust) (model.TrustEntry, error)
	// Reconcile asks the channel's adapter, now, what became of the
	// unresolved attempt of an outcome_unknown operation, and returns the
	// operation as Get shows it afterwards. Without it the runtime asks on
	// its own schedule (Operation.NextAttemptAt); Reconcile does not wait
	// for that time and does not change it for an answer that settles
	// nothing.
	//
	// It is authorized as the current principal for model.ActionOperationRead,
	// exactly as Get, and then for model.ActionOperationReconcile, both on
	// the operation at its current revision; a denial of either is
	// model.Unauthorized and nothing is revealed or called. An operation of
	// another tenant, or one never stored, is model.NotFound.
	//
	// The result is the adapter's answer and never an inference:
	//
	//   - The effect is applied: the operation settles (provider_accepted or
	//     delivered), with the provider's references.
	//   - The effect is reported failed for good: it settles failed or
	//     rejected.
	//   - The provider holds no record of it, past the adapter's
	//     Descriptor.ReconcileSettle window: it was not applied, and the
	//     operation becomes retry_pending. Reconcile sends nothing; the
	//     runtime retries it at Operation.NextAttemptAt (now, when nothing
	//     was scheduled).
	//   - The answer proves nothing, such as "not found" inside the settle
	//     window: the operation comes back unchanged, still outcome_unknown.
	//   - The call fails (an unreachable provider, a failed capability
	//     check): its error is returned and nothing is recorded. An adapter
	//     that does not declare reconcile, or a channel reporting it
	//     unavailable for good (such as disabled by its configuration), is
	//     model.Unsupported.
	//
	// It is idempotent. A settled operation is returned as it is, without a
	// provider call, so repeating Reconcile, or calling it after the runtime
	// settled the operation, is safe. An operation that is neither settled
	// nor outcome_unknown (queued, retry_pending, dead_lettered, held), or whose
	// latest attempt is already answered, is model.Conflict: the runtime,
	// Replay or a human decision owns it. So is one a worker is executing right now; retry once
	// the worker is done. It takes no operation ID and claims no operation
	// key, and it appends a delivery.updated event only when the state
	// changes.
	Reconcile(context.Context, model.Lookup[model.OperationRef]) (model.Operation, error)
	// Lookup recovers an operation after a lost response: the command was
	// sent, its acceptance never arrived, so its operation ref is unknown.
	// It finds the operation by the key the command claimed, which is
	// composite: the principal's tenant and actor, Kind, Target and ID
	// (the command's Operation). The request carries no tenant or caller:
	// a caller finds only its own operations.
	//
	// Kind and Target are those on the mutating method's "Operation key"
	// line:
	//
	//	Configuration.ConfigureChannel  channel.configure        channel ref; creation: model.CreationTarget(tenant, model.KindChannel)
	//	Configuration.ConfigureInbox    inbox.configure          inbox ref; creation: its channel ref
	//	Contacts.Create                 contact.write            model.CreationTarget(tenant, model.KindContact)
	//	Contacts.Update                 contact.write            contact ref
	//	Contacts.Link                   identity.link            identity ref (link and relink alike)
	//	Contacts.Unlink                 identity.unlink          identity ref
	//	Contacts.Merge                  contact.merge            contact ref merged (From)
	//	Conversations.Transition        conversation.transition  conversation ref
	//	Conversations.Start             conversation.start       inbox ref
	//	Conversations.AddLabels         conversation.label       conversation ref
	//	Conversations.RemoveLabels      conversation.label       conversation ref
	//	Conversations.Assign, Unassign  conversation.assign      conversation ref
	//	Conversations.Snooze, Unsnooze  conversation.snooze      conversation ref
	//	Conversations.Link, Unlink      conversation.link        conversation ref
	//	Conversations.AddParticipant    conversation.membership  conversation ref
	//	Conversations.RemoveParticipant conversation.membership  conversation ref
	//	Conversations.Archive, Move, SetRead, Flag, Trash   conversation.state_account or conversation.state_shared (by reported scope)   conversation ref
	//	Labels.Configure                label.configure          label ref; creation: model.CreationTarget(tenant, model.KindLabel)
	//	Labels.Delete                   label.configure          label ref
	//	Messages.Reply                  message.reply            conversation ref
	//	Messages.AddInternalNote        message.note             conversation ref
	//	Messages.Edit                   message.edit             message ref
	//	Messages.Delete                 message.delete           message ref
	//	Messages.React                  message.react            message ref
	//	Messages.Receipt                message.receipt          message ref
	//	WorkItems.Act                   work_item.act            work item ref
	//	Tasks.Act                       task.act                 task ref; opening: conversation ref
	//	Operations.Replay               operation.replay         ref of the replayed operation
	//	Attachments.Prepare             attachment.prepare       inbox ref
	//	Attachments.Upload              attachment.upload        attachment ref
	//	Extensions.Execute              definition's AuthorizeAs request Target
	//	Streams.Start                   stream.write             conversation ref
	//	Streams.Update, End             stream.write             stream ref
	//	Interactions.Respond            interaction.respond      interaction ref
	//	Interactions.Ephemeral          message.ephemeral        conversation ref
	//	Privacy.Redact                  privacy.redact           message, interaction or attachment ref
	//	Privacy.Redact with Cascade     privacy.redact_cascade   contact, identity or conversation ref
	//	Privacy.Export                  privacy.export           filter's conversation, else contact, else first inbox
	//
	// Events.Open, Ack and Close are not listed: Open is idempotent per
	// consumer name and Ack is token-guarded, so repeating the call
	// recovers; Operations.Reconcile is idempotent and claims no key.
	// Methods that return model.Unsupported claim no key.
	//
	// Lookup is authorized as model.ActionOperationRead on the found
	// operation at its revision, exactly as Get, and returns what Get
	// returns. It is read-only: it writes nothing and emits no event.
	//
	// model.NotFound covers a key never claimed, one claimed by another
	// caller or in another tenant, and one purged past
	// RetentionPolicy.Operations; a Target in another tenant is NotFound
	// before any read. NotFound after a purge does not mean the command was
	// never accepted: reusing the operation ID claims the key again only
	// once it is gone, so the Operations horizon is the recovery window.
	// Kind is checked for shape only; a kind no method uses finds nothing
	// and is model.NotFound, not model.Validation.
	Lookup(context.Context, model.OperationLookup) (model.Operation, error)
}

// Events delivers committed domain events at least once through
// subscriptions. Poll returns one leased batch per subscription and Ack
// spends its token to advance the checkpoint. Pollers sharing an actor
// share the subscription's lease: a second Poll while a batch is
// outstanding returns that same batch and token, and only the first Ack
// succeeds; later Acks of the token fail with model.Conflict. Use distinct
// consumer names for independent consumers. List and Get read the log without
// a subscription.
type Events interface {
	Open(context.Context, model.OpenSubscription) (model.SubscriptionResult, error)
	Poll(context.Context, model.Poll) (model.EventBatch, error)
	Ack(context.Context, model.Acknowledge) (model.SubscriptionResult, error)
	Close(context.Context, model.Lookup[model.SubscriptionRef], model.Command) (model.Acceptance, error)
	// List reads one page of the tenant's committed events without a
	// subscription: it creates, leases and advances no subscription state
	// and writes nothing.
	//
	// Each call is authorized for model.ActionEventList on
	// model.EventLogRef(tenant); a denial is model.Unauthorized and nothing
	// is read. Each scanned event is then authorized as Poll authorizes
	// it, and events the principal may not read are skipped without trace.
	//
	// One call makes one store read of req.Limit events
	// (DefaultEventListLimit when zero, capped at MaxEventListLimit): Limit
	// bounds the events scanned, the ones the filter keeps, not the events
	// returned. Events the principal may not read count against it and are
	// dropped after, so a page may be short or empty and still advance. Next is the log position after the last scanned event, also
	// on an empty page; Complete reports that the read reached the head.
	// Positions are those of Subscription.Checkpoint: list up to Next, then
	// Open with model.StartCheckpoint at Next to continue without a gap or
	// a duplicate.
	//
	// req.Start chooses the first position: model.StartBeginning (the
	// retained beginning), model.StartTail (the head: an empty, complete
	// page whose Next is the head) or model.StartCheckpoint at req.After.
	// Next is empty only on a log never written: continue with
	// model.StartBeginning, since model.StartCheckpoint needs an After.
	// The filter honours Inboxes and Kinds, as subscriptions do, and
	// Conversation and Since, which subscriptions refuse: any other set
	// field and an unknown kind are model.Validation, an inbox or a
	// conversation of another tenant model.NotFound. Conversation selects
	// the events about the conversation, its messages, tasks, work items
	// and interactions (store.EventConversation); an event about no
	// conversation is never selected while it is set. Since selects the
	// events whose RecordedAt is at or after it, not OccurredAt as
	// Messages.List does; with model.StartCheckpoint the read starts at the
	// later of After and the first position Since can select, so resuming
	// at Next with the same filter neither repeats nor skips an event. A
	// position behind event retention is model.CursorExpired: restart the
	// list from beginning or tail and reconcile from current resource
	// state.
	//
	// Events are returned as stored. List declares no payload major, unlike
	// Open: each event's Payload names its schema and version, and a
	// consumer skips or rejects a version it does not read.
	List(context.Context, model.ListEvents) (model.Page[model.Event], error)
	// Get reads one committed event by ID without a subscription: it
	// creates, leases and advances no subscription state and writes nothing.
	//
	// The call is authorized for model.ActionEventList on
	// model.EventLogRef(tenant), as List is; a denial is model.Unauthorized
	// and nothing is read. The event is then authorized as Poll and List
	// authorize each event they scan: the principal must read its inbox and
	// the resource it is about, at the event's audience. Whatever the reason
	// the principal may not read it, the answer is the one for an event that
	// does not exist: model.NotFound, with nothing that tells the two apart.
	// A ref of another tenant and an event removed by event retention are
	// model.NotFound too, so NotFound means only that this principal cannot
	// read such an event now. An authorization failure of the policy itself is
	// not NotFound: it fails the call as it does List.
	//
	// The event is returned as stored, like List's: its Payload names its
	// schema and version.
	Get(context.Context, model.Lookup[model.EventRef]) (model.Event, error)
}

// Attachments stages and reads attachment bytes. Open authorizes every
// call against current state: model.ActionAttachmentRead under the
// attachment's own audience and, while messages hold it, through one of
// them: model.ActionMessageRead on the message and
// model.ActionAttachmentRead under the message's audience. An attachment
// no message holds is readable only by the actor that prepared it. Making
// every message holding an attachment internal therefore makes the
// attachment unreadable to callers limited to public content; a denial is
// model.Unauthorized.
type Attachments interface {
	// Prepare declares an attachment in an inbox before its bytes.
	// Operation key: kind attachment.prepare, target the inbox ref.
	Prepare(context.Context, model.PrepareAttachment) (model.Acceptance, error)
	// Upload writes a prepared attachment's bytes.
	// Operation key: kind attachment.upload, target the attachment ref.
	Upload(context.Context, model.Lookup[model.AttachmentRef], model.Command, io.Reader) (model.Acceptance, error)
	Open(context.Context, model.Lookup[model.AttachmentRef]) (model.Attachment, io.ReadCloser, error)
}
type Extensions interface {
	Query(context.Context, model.QueryExtension) (model.Extension, error)
	// Execute runs an extension effect.
	// Operation key: kind the definition's AuthorizeAs, target the
	// request's Target.
	Execute(context.Context, model.ExecuteExtension) (model.Acceptance, error)
}

// Interactions reads accepted interaction records and answers them, and
// sends recipient-bound ephemeral replies and operational signals on a
// conversation's stored route. Ephemeral is a queued operation authorized
// for model.ActionMessageEphemeral and needs the send_ephemeral
// capability; Signal is one synchronous provider call authorized for
// model.ActionConversationSignal, needs the signal capability and records
// nothing.
type Interactions interface {
	// Get returns an interaction record, authorized for
	// model.ActionInteractionRead under its audience. The record never
	// holds the response token. A foreign or unknown interaction is
	// model.NotFound.
	Get(context.Context, model.Lookup[model.InteractionRef]) (model.Interaction, error)
	// List pages interaction records in recording order, optionally
	// limited by Filter.Inboxes, Filter.Conversation and Filter.Identity
	// (the actor), each authorized for model.ActionInteractionRead under
	// its own audience; records the caller may not read are skipped
	// without trace.
	List(context.Context, model.ListRequest) (model.Page[model.Interaction], error)
	// Respond answers an interaction through the adapter that reported
	// it, in an explicit mode and audience, authorized for
	// model.ActionInteractionRespond on the interaction under its audience
	// and needing the respond capability (respond_follow_up for a
	// follow-up; model.Unsupported otherwise). Mode message commits a
	// message with the operation, correlated with the interaction; mode
	// ephemeral requires a restricted audience and stores no message; mode
	// update replaces the provider message the control sits on and stores
	// none. ExpiresAt bounds the attempt and may not pass the
	// interaction's RespondUntil when that is set.
	//
	// The initial response is claimed atomically with the operation: of
	// two concurrent initial responses one is accepted and the other is
	// model.Conflict, as is a response to a redacted interaction. A
	// follow-up counts one and may precede the initial response. A
	// response whose window or own deadline has passed is
	// model.DeadlineExpired, at submit and again at the attempt, which
	// then makes no provider call and never falls back to another mode.
	// The same operation ID and payload replays the accepted response. The
	// response token is read at the attempt and given to the adapter; it
	// is never stored in the operation, its evidence or any event.
	// Operation key: kind interaction.respond, target the interaction ref.
	Respond(context.Context, model.Respond) (model.Acceptance, error)
	// Ephemeral sends an expiring message to one recipient identity. One
	// naming an Interaction is that interaction's follow-up response to
	// its actor: it needs respond_follow_up besides send_ephemeral and
	// interaction.respond on the interaction, counts one follow-up and
	// never claims the initial response; the token is used only then.
	// Operation key: kind message.ephemeral, target the conversation ref.
	Ephemeral(context.Context, model.Ephemeral) (model.Acceptance, error)
	Signal(context.Context, model.Signal) error
}

// Streams runs logical streaming responses: Start opens a stream in a
// conversation, each Update carries the whole response so far under the
// next sequence, and End finishes or cancels it. Each command is its own
// operation, authorized for model.ActionStreamWrite; Get is authorized for
// model.ActionStreamRead under the stream's audience. The mode decides the
// capabilities the channel's adapter must declare: stream for native,
// send and edit for edit, send for buffer.
type Streams interface {
	Get(context.Context, model.Lookup[model.StreamRef]) (model.Stream, error)
	// Start opens a stream in a conversation.
	// Operation key: kind stream.write, target the conversation ref.
	Start(context.Context, model.StartStream) (model.Acceptance, error)
	// Update appends to a stream.
	// Operation key: kind stream.write, target the stream ref.
	Update(context.Context, model.StreamUpdate) (model.Acceptance, error)
	// End ends or cancels a stream.
	// Operation key: kind stream.write, target the stream ref.
	End(context.Context, model.EndStream) (model.Acceptance, error)
}

// Privacy redacts and exports retained content.
//
// Redact replaces a message, interaction or attachment with a tombstone, as a committed
// operation authorized for model.ActionPrivacyRedact on the target under
// its own audience. ExpectedRevision must equal the target's revision; the
// tombstone takes a new one. A message keeps its references, author,
// audience, origin and times; its content and provider metadata go, as do
// the copies in the attachments it references (bytes included), the raw
// ingress records of its provider message IDs and its originating
// operation's intent, while deduplication and replay correlation remain
// for their retention horizon. A message whose originating operation still
// has work, such as a queued reply, is model.Conflict. A message redaction
// emits message.updated with change redacted, and never cascades.
//
// An interaction (Redact.Interaction) loses its submitted values, its
// arguments and its response token, and the raw ingress record that
// created it loses the same; it keeps its references, actor, kind,
// identifiers and response state, takes Redacted and a new revision, and
// emits interaction.updated with change redacted.
//
// With Cascade, Redact takes a contact, identity or conversation target
// and queues a durable operation that redacts every message and
// interaction it owns (a conversation's, or those the contact's identities
// or the identity authored or performed, in any inbox) with the messages'
// attachments, whatever their number. Submission is authorized for model.ActionPrivacyRedactCascade on
// the target and compares its revision. A contact cascade then decides
// model.ActionPrivacyRedact on each identity on the contact, whatever its
// inbox, under the threshold Config.CascadePolicy sets per tenant and
// inbox (see policy.CascadePolicy): the most restrictive threshold of the
// contact's inboxes applies to the whole contact. Threshold 0 refuses the
// submission; N refuses it once N identities are denied; -1, the default,
// never refuses on denials. A refusal is model.Unauthorized with nothing
// written. Otherwise a denied identity is skipped: its messages,
// attachments and link history are left as they are, never redacted,
// and the operation's Progress.Partial is set, without saying which
// identities, how many or in which inboxes. The submission tombstones, in
// its own unit, the link history of the identity or of each redacted
// identity on the contact: every model.IdentityLink loses its evidence
// facts and takes Redacted, keeping its class, contacts, verifier,
// evidence reference, reason and provenance, so Contacts.History still
// shows what moved and a consumed evidence reference stays consumed. The
// identities a cascade redacts are fixed at submission: a retry replays
// the stored operation and never adds one skipped then. Message, attachment and conversation redaction never touch
// link history, and Export does not include it. Runtime.Run workers then
// redact the messages in bounded steps, each rechecking
// model.ActionPrivacyRedact on its records as the originating actor and
// resuming where the last committed step stopped after a crash.
// Operations.Get reports progress: the operation is model.Committed once
// every owned message and interaction is redacted, or model.Failed with
// code model.Unauthorized at the first record the actor may not redact,
// Progress.Done counting exactly the records redacted; link history
// tombstoned at submission stays so. The target record itself is left as
// it is, except that a contact target loses its display name and attributes
// (Contacts.Create and Update) in the submission's unit: the contact, its
// kind and its identities stay, under a new revision. PropagateToProvider
// is model.Unsupported in this version.
//
// Export writes the records its filter selects to a restricted artifact:
// a tenant-scoped export resource, authorized for model.ActionPrivacyExport
// on the filter's subject, holding JSONL lines of the records the caller
// may read, each under its own audience (interactions under
// model.ActionInteractionRead, never with a response token), and the definition of every label
// an exported conversation holds that the caller may read
// (model.ExportLineLabel). The acceptance's result is the export's
// reference; no path or URL is ever returned. Open authorizes the
// subject again on every call and reauthorizes every record the export
// holds against its current state, as a direct read of it would be; when
// the caller may not read one of them now, or one is gone, Open is
// model.Unauthorized before any byte and the caller exports again. An
// export holding a message, interaction or attachment redacted since it was built is
// model.Conflict before any byte, likewise re-exported. Otherwise Open
// streams the verified bytes. Exports are kept for
// RetentionPolicy.Exports.
type Privacy interface {
	// Redact tombstones a message, interaction or attachment, or with Cascade queues
	// the redaction of everything a contact, identity or conversation owns.
	// Operation key: kind privacy.redact, target the message, interaction
	// or attachment ref; with Cascade, kind privacy.redact_cascade, target the contact,
	// identity or conversation ref.
	Redact(context.Context, model.Redact) (model.Acceptance, error)
	// Export writes the records its filter selects to an export artifact.
	// Operation key: kind privacy.export, target the filter's conversation
	// ref, else its contact ref, else its first inbox ref.
	Export(context.Context, model.ExportRequest) (model.Acceptance, error)
	Open(context.Context, model.Lookup[model.ExportRef]) (model.Export, io.ReadCloser, error)
}
type RuntimeCall struct {
	Principal model.Principal
	Call      model.Call
}

// WorkerOptions configures one Runtime.Run call. Owner names the lease
// holder and is required. Limit caps work items per claim round (default
// 1). Kinds selects work kinds; empty selects every kind the runtime
// executes. Lease is the claim duration (default 1m) and Poll the wait
// between rounds that found no work (default 1s). SweepEvery is how often
// Run expires held operations past their deadline (default 1m, negative
// turns the sweep off); it is also the window after which an operation
// still held is marked carried over (model.HeldBlock.CarriedOverAt).
type WorkerOptions struct {
	Owner      string
	Limit      int
	Kinds      []string
	Lease      time.Duration
	Poll       time.Duration
	SweepEvery time.Duration
}

// ReceiveOption configures one Runtime.Receive call.
type ReceiveOption struct {
	owner     string
	spread    time.Duration
	spreadSet bool
}

// ReceiveOwner names the Receive call as the holder of the channel's
// receiver lease, as Runtime.ReceiverStatus reports it, such as a host and
// process name. It follows the opaque-token rule. Without it the call is
// named receiver/<id> with a fresh ID.
func ReceiveOwner(owner string) ReceiveOption { return ReceiveOption{owner: owner} }

// ReceiveStartSpread delays the call's first connection by a stable offset
// in [0, d) taken from a hash of the channel, so receivers a host starts
// together do not open their first connections in phase. The same channel
// always gets the same offset; reconnects are not delayed. Zero (the
// default) starts at once; a negative d fails the call with
// model.Validation. The lease is held and renewed through the wait.
func ReceiveStartSpread(d time.Duration) ReceiveOption {
	return ReceiveOption{spread: d, spreadSet: true}
}

// ReceiverStatus is a channel's receiver lease: the Owner of the Receive
// call holding it and Until, when the lease expires unless renewed. A
// channel no Receive owns has no Owner and a zero Until.
//
// The other fields are what the process running that Receive call knows of
// it, read from its memory by a Runtime.ReceiverStatus of the same runtime.
// They are zero for a call another process runs (ask the Owner), for an
// expired lease and for a call that has returned. They are not stored: a
// crash loses them, and the next Receive starts at ReceiverRunning with no
// failures. They are advisory and change no retry or lease decision.
type ReceiverStatus struct {
	Owner string
	Until time.Time
	// Phase is what the call is doing and Since when that began.
	Phase ReceiverPhase
	Since time.Time
	// Failures is the consecutive connections that accepted nothing, the
	// count RetryPolicy.MaxAttempts bounds; zero once a connection accepted
	// ingress. Reason is the code of the last of them, empty when none; it
	// carries no provider message.
	Failures int
	Reason   model.ErrorCode
	// RetryAt is, while retrying, the earliest time the next connection
	// opens by the engine's clock: the backoff, or the wait the failure
	// stated, up to 24 hours.
	RetryAt time.Time
	// Link is what the receive adapter says of its provider link while the
	// phase is ReceiverRunning: ReceiverConnecting, ReceiverConnected or
	// ReceiverDegraded.
	// It is empty when the adapter does not report it
	// (adapter.ReceiverReporter) and outside the running phase.
	Link ReceiverLink
	// Detail is, while Link is ReceiverDegraded, the adapter's stable code
	// for what is failing, such as "google.mail.push_denied"; empty
	// otherwise. It is a code, never a provider message.
	Detail string
}

// ReceiverPhase is what a Receive call is doing.
type ReceiverPhase string

// Receiver phases.
const (
	// ReceiverRunning: a connection is being opened or is open. It does not
	// say the provider link is up; Failures above zero while running means
	// the call is reconnecting after failures, and Link says more when the
	// adapter reports it.
	ReceiverRunning ReceiverPhase = "running"
	// ReceiverRetrying: the last connection failed and the call waits,
	// until RetryAt, to open the next.
	ReceiverRetrying ReceiverPhase = "retrying"
	// ReceiverEnded: the call stopped and is releasing its lease. It is seen
	// only until Receive returns, which also returns the reason.
	ReceiverEnded ReceiverPhase = "ended"
)

// ReceiverLink is what the receive adapter reports of its provider link.
type ReceiverLink string

// Receiver links.
const (
	// ReceiverConnecting: the adapter is not connected yet, or is
	// re-establishing its link inside the connection.
	ReceiverConnecting ReceiverLink = "connecting"
	// ReceiverConnected: the adapter says its provider link is up.
	ReceiverConnected ReceiverLink = "connected"
	// ReceiverDegraded: the adapter says it still receives but part of what
	// it relies on is failing; Detail has its code. The link returns to
	// ReceiverConnecting or ReceiverConnected when the adapter reports
	// recovery (adapter.ReceiverDegradationReporter).
	ReceiverDegraded ReceiverLink = "degraded"
)

// Runtime runs the engine's receivers and workers. Webhook and Receive bind
// the channel's sink to the call's service principal, which needs
// model.ActionChannelReceive on the channel to bind and again, with the
// resolved inbox as destination, for every ingress; a denial is
// model.Unauthorized and writes nothing.
type Runtime interface {
	Webhook(context.Context, RuntimeCall, model.ChannelRef) (http.Handler, error)
	// Receive runs the channel's persistent receiver; one call owns a
	// channel at a time, and another call's Receive is model.Conflict.
	Receive(context.Context, RuntimeCall, model.ChannelRef, ...ReceiveOption) error
	// ReceiverStatus reports who holds the channel's receiver lease and
	// until when, without starting a Receive, and what the Receive of this
	// process is doing (see ReceiverStatus).
	ReceiverStatus(context.Context, RuntimeCall, model.ChannelRef) (ReceiverStatus, error)
	// ReceiverState is the state of the Receive this runtime runs for the
	// channel, from memory alone: it reads no store, so a host can poll it
	// per channel. It reports false when this runtime runs no Receive for
	// the channel (another process's, one that returned), and for a call
	// whose principal is invalid or of another tenant. Until is zero; Owner
	// and the fields of ReceiverStatus are the local call's.
	ReceiverState(RuntimeCall, model.ChannelRef) (ReceiverStatus, bool)
	Run(context.Context, RuntimeCall, WorkerOptions) error
	// Step runs one claim round in the caller's goroutine and returns how
	// many work items it claimed and executed: at most WorkerOptions.Limit
	// items of the selected kinds, each to its next durable state. It never
	// waits and starts no goroutine, so an empty round returns 0 at once
	// and WorkerOptions.Poll is unused; the caller owns the loop and the
	// clock. Tests use it to advance work deterministically, and recovery
	// tools to finish work a stopped worker left. It takes the same
	// options and has the same effects as one round of Run: an item whose
	// lease expired is claimed again, and a round the work policy's
	// in-flight cap left empty fails with model.Unavailable. Drain and
	// Close apply as to Run: a drained tenant claims nothing, and Drain
	// waits for a Step in flight.
	Step(context.Context, RuntimeCall, WorkerOptions) (int, error)
	Drain(context.Context, RuntimeCall) error
	// Purge deletes the caller's tenant's records past the Config.Retention
	// horizons, in bounded units of work, and the staged blobs no upload
	// bound within the Staging horizon, and reports what it deleted. The
	// host schedules it; nothing purges on its own.
	Purge(context.Context, RuntimeCall) (PurgeReport, error)
	// Import reads a bounded window of the provider's past messages, of a
	// channel or of one conversation, through the channel adapter's
	// adapter.HistoryReader, and records each as inbound, with
	// message.received like any ingress, so helpdesk-style clients can
	// fill a conversation that started before uma saw it.
	//
	// Each message it records carries model.Message.ImportedBy, set to
	// req.Requester. Imported history is old news for desk state: it never
	// wakes a snooze (Conversations.Snooze, Filter.Snoozed) and never
	// makes a conversation unread for anyone (Filter.Unread,
	// model.ReadMarker.Unread); only messages arriving live do.
	//
	// req.Requester must hold model.ActionChannelImport on the channel, or
	// on the conversation for a conversation import, checked first; then
	// the call's service principal binds the channel's sink with
	// model.ActionChannelReceive on the channel and again into the
	// resolved inbox for every message, as Receive. A denial is
	// model.Unauthorized before any provider call or write. The adapter
	// must declare model.CapabilityHistory, else model.Unsupported. A
	// reference in another tenant, or to nothing, is model.NotFound.
	//
	// A message is imported once, by its provider message ID: one the
	// destination inbox already holds, however it arrived, is skipped and
	// left as it is, so a second import of the same window imports
	// nothing. The call reads at most req.Limit envelopes; the report says
	// what it imported, skipped and found missing, and how to resume. A
	// failing read or ingress ends the call with its error and the report
	// of what was done before it. Concurrent imports of one window may
	// both count a message the provider assigns one ID to; it is still
	// recorded once.
	Import(context.Context, RuntimeCall, ImportRequest) (ImportReport, error)
}
