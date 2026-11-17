package conformance

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// RunAdapterSuite runs the shared provider adapter cases against h. Every
// case composes a fresh engine over a fresh store with the adapter
// registered like any third-party factory, and drives it through the public
// engine API only: Runtime.Receive for inbound traffic, Messages.Reply,
// Extensions.Execute and Runtime.Run for outbound effects.
//
// The suite runs only the cases the declared capabilities imply. Contract
// cases always run: the descriptor matches h.Capabilities, an opened
// instance implements exactly the interfaces of its declared builtin
// capabilities, no more and no fewer, and its Capabilities report names
// every declared capability exactly once, available without a reason or
// unavailable with one: the engine treats an unreported capability as
// unavailable. Then, per capability:
//
//   - receive, or receive_http with a WebhookProvider: normalization into
//     the bound sink with stable provider references, duplicate
//     redelivery, and the inbound policy-bypass cases (bound inbox only,
//     selector results stay inside the candidate snapshot, audience never
//     widened, no identity from payload metadata), driven through
//     Runtime.Receive, or through the handler of Runtime.Webhook for an
//     adapter receiving over HTTP only;
//   - send: accepted, rejected, transient and lost-answer outcomes mapped to
//     operation states, Retry-After, credentials resolved per attempt and
//     never disclosed, revoked credentials and permissions, isolation of two
//     accounts of one type and the stored-route bypass case;
//   - send and reconcile: reconcile before any retry;
//   - extension_execute: a typed extension action round-trips fields the
//     engine does not know and keeps its audience, an incompatible action
//     is rejected before any provider call, and a replayed operation ID
//     returns the operation the provider call carried; malformed or
//     oversize action data is refused at the call with nothing stored; an
//     InvalidAction is never applied by the provider, and is refused at the
//     call with nothing stored when the factory implements
//     adapter.ActionValidator;
//   - receive with an EventProvider and AdapterHarness.Event: a provider
//     notice of a registered event kind reaches a subscriber once, about
//     the message it names, and a notice of an unregistered kind is
//     refused;
//   - stream: a native stream's start, ordered updates and finish or
//     cancel on one provider stream, ends that happen once, and partial
//     outcomes; send and edit: an edit-mode stream; send: a buffered one;
//   - send_ephemeral: an ephemeral reply to a stored participant that is
//     never stored as a message nor attempted after its deadline;
//   - signal: a signal sent once and never recorded;
//   - work_item_act with a WorkItemProvider: every transition the
//     descriptor declares is honoured, with each declared reason and with
//     the adapter's default, on the work item's reference; a transition or
//     reason it does not declare is model.Unsupported before any provider
//     call; a closed status reopens only where declared; a stale local
//     status is still sent to the provider; rejected, transient,
//     rate-limited, lost-answer and lost-request outcomes map to operation
//     states, a lost answer is reconciled and never sent again;
//   - edit, delete and react: a mutation targets the message's stored
//     provider message and changes the message only once the provider
//     applied it, never while it is pending (the operation shows the
//     pending change instead); an edit records its editor and keeps the
//     audience, a delete leaves a tombstone, a reaction is added and then
//     removed; a rejected, refused or lost-answer mutation leaves the
//     message untouched and is never resent; an inbound edit that lands
//     before the provider's answer wins; with receive, a provider echo
//     naming the attempt settles a mutation whose answer was lost, edits
//     of an imported message are allowed and a reaction summary stores
//     counts only; a revoked permission makes no call, a message outside
//     the caller's scope is model.NotFound and makes no call;
//   - an adapter whose Events declare interaction.received, receiving
//     over either path: a provider interaction is recorded as an interaction
//     and never as a message, once however often it is redelivered, one
//     interaction.received event, its actor the authenticated sender and
//     never a payload claim, a source message of the inbox joined, a
//     deadline that already passed still recorded, the response token
//     stored beside the record and in no read, event or export, and
//     redaction clearing its values and its token;
//   - respond: a response in each mode and audience reaches the provider
//     with the interaction and its token, the initial response has one
//     winner, a replayed response reuses its result, a window that closed
//     is model.DeadlineExpired explicitly; with respond_follow_up,
//     follow-ups count and leave the initial response open;
//   - receipt, with inbound traffic: each receipt kind the provider sends
//     reaches it once, on the received message's stored provider message
//     and the conversation's route, and changes nothing in uma; a retried
//     submission is replayed and never sent again; a kind listed in
//     AdapterHarness.UnsupportedReceiptKinds settles rejected with
//     model.Unsupported before any provider call; a rejected receipt is
//     final, a lost answer is never resent and is reconciled with
//     reconcile, a revoked permission makes no call;
//   - membership: a participant is added and then removed at the provider
//     on the conversation's route, as the identity the adapter reports
//     inbound, and joins or leaves the conversation only once the provider
//     applied the change; a retried submission is replayed and never sent
//     again; a rejected change leaves the conversation as it was, a lost
//     answer is never resent and is reconciled with reconcile, a revoked
//     permission makes no call;
//   - archive, move, read_state, flag and trash: each verb a declared
//     capability carries (archive and unarchive, move, read and unread,
//     flag and unflag of every kind reported, trash and untrash) reaches
//     the provider once on the conversation's route, naming the provider
//     messages of the conversation and no target, and is recorded in the
//     conversation's provider state only once the provider applied it,
//     never while pending; every state capability reports parseable limits
//     (scope, flag kinds, move shape, destination roles) and the
//     operation is authorized as the reported scope; a move is held to the
//     reported shape (a folder-shaped provider takes a destination, never
//     label changes), a role it does not resolve and a flag kind it does
//     not report are model.Unsupported before any provider call; a
//     retried submission is replayed and never sent again, a state the
//     provider holds already is applied; a rejected change is final, a lost
//     answer is never resent and is reconciled with reconcile (a move found
//     in its destination is applied), a revoked permission makes no call;
//     a move that gives items new references has them added to the stored
//     messages and the next state change names them; with a StateProvider
//     and inbound traffic, the provider's own report of a conversation's
//     state is applied once, a stale or unknown one changes nothing, never
//     touches a read marker, and the report of an unsettled change settles
//     its operation without sending it again; a capability that is not
//     declared is model.Unsupported with no provider call;
//   - history with a HistoryProvider: the reader returns, in pages of at
//     most the requested limit, exactly the held messages of the window,
//     oldest first, each once, with the provider message ID and
//     destination the provider reported; one conversation's messages for
//     a route, the channel's without one; the same pages when read again,
//     whatever the page size; nothing applied at the provider; a cursor
//     the provider never issued is model.Validation; with inbound traffic,
//     Runtime.Import skips a message ingress already holds and records
//     the others once;
//   - raw_read with a RawProvider and inbound traffic: Messages.Raw returns
//     the provider's whole copy of a received message, byte for byte, read
//     again at each call, with nothing applied at the provider and nothing
//     changed in uma; a message uma does not hold is model.NotFound; a
//     channel that does not declare it is model.Unsupported with no
//     provider call;
//   - send without receive, receive without send, and any missing stream,
//     send_ephemeral, signal, work_item_act, edit, delete, react, respond or
//     respond_follow_up capability: the request fails with
//     model.Unsupported and no provider call, never falling back to
//     another capability.
//
// A case skips when the harness reports ErrNotExpressible for the scenario
// it needs. A provider reached over HTTP implements HTTPProvider: its client
// is the engine's, as a host injects it. Run it from the adapter's module:
//
//	func TestAdapterConformance(t *testing.T) {
//		conformance.RunAdapterSuite(t, conformance.AdapterHarness{
//			New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
//				f := myadapter.NewFactory()
//				return f, newScriptedProvider(f)
//			},
//			Capabilities: []model.Capability{model.CapabilityReceive, model.CapabilitySend},
//		})
//	}
func RunAdapterSuite(t *testing.T, h AdapterHarness) {
	t.Helper()
	if h.New == nil || len(h.Capabilities) == 0 {
		t.Fatal("AdapterHarness needs New and Capabilities")
	}
	declared := func(c model.Capability) bool { return slices.Contains(h.Capabilities, c) }
	for _, g := range adapterGroups() {
		t.Run(g.name, func(t *testing.T) {
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					for _, need := range c.needs {
						if !declared(need) {
							t.Skipf("capability %s not declared", need)
						}
					}
					if len(c.anyOf) > 0 && !slices.ContainsFunc(c.anyOf, declared) {
						t.Skipf("none of capabilities %v declared", c.anyOf)
					}
					for _, absent := range c.absent {
						if declared(absent) {
							t.Skipf("capability %s declared", absent)
						}
					}
					e := newAdapterEnv(t, h)
					for _, kind := range c.events {
						if !slices.Contains(e.desc.Events, kind) {
							t.Skipf("event kind %s not declared", kind)
						}
					}
					if c.inbound {
						e.requireInbound(t)
					}
					c.run(t, e)
				})
			}
		})
	}
}

// AdapterHarness is one adapter implementation under test and the provider
// side behind it.
type AdapterHarness struct {
	// New returns a fresh factory and the provider it talks to, for one
	// case. Required. It may register cleanup with t.Cleanup.
	New func(t *testing.T) (adapter.Factory, Provider)
	// Capabilities is what the factory is expected to declare. Required:
	// the suite fails when Describe reports another set, so a dropped
	// declaration cannot silently skip its cases.
	Capabilities []model.Capability
	// Selection is the inbox selection inbound cases configure. The zero
	// value is per-contact selection, locked to a single conversation,
	// under model.RuleSetContactLifecycleV1.
	Selection model.Selection
	// Store returns a fresh, empty store judging leases by clock. Nil uses
	// the memory store.
	Store func(t *testing.T, clock ports.Clock) store.Store
	// Extensions are the extension definitions the adapter ships, as a
	// host registers them. Every case registers them.
	Extensions []adapter.ExtensionDefinition
	// Action is a valid action of an effect definition in Extensions, with
	// data holding a JSON object, that the provider applies when it
	// accepts. The extension cases add fields the adapter does not know to
	// it; they skip when Action is unset.
	Action model.Extension
	// InvalidAction is an action of the same definition as Action that is
	// valid JSON within the size limit but whose fields the adapter
	// refuses, such as a required field missing. The invalid-fields case
	// requires that the provider applies nothing, and a model.Validation
	// error at the call, with nothing stored, when the factory implements
	// adapter.ActionValidator; it skips when InvalidAction is unset. An
	// adapter with no field rules leaves it unset.
	InvalidAction model.Extension
	// Event is a valid event of an event-kind definition in Extensions the
	// adapter declares, with data holding a JSON object. The extension
	// event case has an EventProvider deliver it and reads it back through
	// a subscription; it skips when Event is unset or the Provider is no
	// EventProvider.
	Event model.Extension
	// NoResponseToken marks an adapter whose provider has no response
	// credential for interactions, such as a channel the adapter itself
	// serves: the interaction cases report no token, store none and expect
	// the provider calls that answer interactions to carry none, and the
	// case on a token's custody skips.
	NoResponseToken bool
	// UnsupportedReceiptKinds are the receipt kinds (model.ReceiptDelivered,
	// model.ReceiptRead) the provider has no receipt for, such as a
	// provider that can mark a message read but never report it delivered.
	// The receipt cases require the adapter to settle a receipt of such a
	// kind rejected with model.Unsupported before any provider call, and
	// every other kind to reach the provider. Nil: the provider sends both.
	UnsupportedReceiptKinds []string
	// MoveChangesReference marks a provider whose move gives an item a new
	// reference, such as an IMAP copy into the destination mailbox. The
	// move cases then require the adapter to report the new reference
	// (adapter.Outcome.ReferenceChanges); without it they require that
	// whenever the provider no longer holds an item under the reference it
	// was moved by.
	MoveChangesReference bool
}

// ErrNotExpressible is returned by a Provider method when the provider
// cannot express the requested scenario, such as a private inbound message
// on a provider without private messages. The case needing it skips.
var ErrNotExpressible = errors.New("conformance: scenario not expressible by this provider")

// Provider is the scripted provider side of an adapter: the accounts behind
// channels, the traffic they receive and the effects they observe. Each
// channel the suite configures is a separate provider account, even when
// two accounts use equal external IDs. Methods are called from the test
// goroutine; Inbound and Calls may be called while the adapter runs.
type Provider interface {
	// Account returns the channel settings of the provider account behind
	// channel: a configuration the factory validates and the credential
	// reference its connections resolve.
	Account(channel model.ChannelRef) Account
	// Route returns the reply route on channel to the remote party peer, as
	// the adapter itself normalizes it: for a per-contact inbox, the sender
	// reference of inbound traffic from peer.
	Route(channel model.ChannelRef, peer string) model.ExternalRef
	// Deliver queues one inbound provider event on channel's account, for
	// the adapter to normalize and hand to its sink.
	Deliver(channel model.ChannelRef, ev InboundEvent) error
	// Inbound reports the events of channel's account the adapter has
	// handed to a sink so far, in order.
	Inbound(channel model.ChannelRef) []InboundResult
	// Respond scripts the provider's answers to the next outbound calls on
	// channel, consumed in call order; once used up the provider accepts.
	Respond(channel model.ChannelRef, rs ...Response) error
	// Calls reports the outbound calls channel's account received, in order.
	Calls(channel model.ChannelRef) []ProviderCall
	// Revoke withdraws the provider permission behind capability c on
	// channel's account.
	Revoke(channel model.ChannelRef, c model.Capability) error
}

// CredentiallessProvider is implemented by the Provider of an adapter that
// resolves no credential for its outbound effects, because it has no
// provider to call: the remote side reads what the adapter records. The
// cases that revoke the channel credential and expect no provider call have
// nothing to revoke and skip.
type CredentiallessProvider interface {
	Credentialless() bool
}

// HTTPProvider is implemented by a Provider the adapter reaches over HTTP.
// The suite composes every engine with its client as Config.HTTP, so the
// adapter's instances reach the provider through Dependencies.HTTP, the
// host-injected client, and never need a client of their own.
type HTTPProvider interface {
	// HTTPClient returns the client reaching the provider of one case.
	HTTPClient() ports.HTTPClient
}

// WebhookProvider is implemented by the Provider of an adapter receiving
// over HTTP (receive_http): it calls the webhook the way the provider calls
// its subscriber. Without it, the inbound cases of an adapter that only
// receives over HTTP skip.
type WebhookProvider interface {
	// Push sends the events Deliver queued on channel's account to h, the
	// handler Runtime.Webhook mounted for the channel, in order, each as
	// the provider's own request, signed or authenticated with
	// CredentialSecret where the provider does so. Every event pushed is
	// reported by Inbound, acknowledged when the handler's answer counts as
	// a success to the provider.
	Push(channel model.ChannelRef, h http.Handler) error
}

// StateProvider is implemented by the Provider of an adapter declaring
// provider state capabilities (archive, move, read_state, flag or trash),
// the provider holding the items the state verbs change. Without it the
// cases that look at the provider's own state, or have it report state,
// skip. A provider item is a message the account holds, named by its
// provider message reference: those uma sent and those it received.
type StateProvider interface {
	// ItemState reports what channel's account holds for the item ref
	// names, and false when it holds no item under that reference, such as
	// one a move gave another.
	ItemState(channel model.ChannelRef, ref model.ExternalRef) (ItemState, bool)
	// DeliverState queues the provider's report of the state of the
	// conversation the item MessageID belongs to, for the adapter to
	// normalize and hand to its sink. It returns ErrNotExpressible when
	// the provider cannot report the state as asked.
	DeliverState(channel model.ChannelRef, ev StateEvent) error
}

// ItemState is the state a provider holds for one item: Folder for a
// provider that holds an item in one folder, Labels (provider label IDs)
// for one that labels it.
type ItemState struct {
	Archived bool
	Trashed  bool
	Read     bool
	Folder   *model.Folder
	Labels   []string
	Flags    []string
}

// StateEvent is a provider report of conversation state, in provider
// terms. State.Channel is ignored; its ObservedAt is the provider's time of
// the report, and Complete says that it names every item of the
// conversation.
type StateEvent struct {
	// EventID is the provider's ID of the report.
	EventID string
	// MessageID names an item of the conversation the report is about, as
	// the adapter reported it: the provider message ID of a message it
	// received or sent, or one a move re-resolved.
	MessageID string
	// Destination is the provider address the report was sent to.
	Destination string
	State       model.ProviderConversationState
}

// WorkItemProvider is implemented by the Provider of an adapter declaring
// work_item_act: the work item cases need work items on the provider to
// act on. Without it they skip, except those that never reach the provider.
// Its ProviderCalls of capability model.CapabilityWorkItemAct carry
// Status, Reason and the work item's Route.
type WorkItemProvider interface {
	// SeedWorkItem creates on channel's account the provider's work item
	// for peer, holding the normalized status, and returns its reference:
	// the one Route returns for the peer. It returns ErrNotExpressible when
	// the provider cannot hold a work item in that status.
	SeedWorkItem(channel model.ChannelRef, peer string, status string) (model.ExternalRef, error)
	// WorkItemStatus reports the status the provider holds for work, as a
	// normalized status.
	WorkItemStatus(channel model.ChannelRef, work model.ExternalRef) (string, error)
}

// EventShaper is implemented by a Provider whose adapter derives an event's
// provider IDs from more than one fixture value, such as a message ID
// scoped to its sender. ShapeEvent returns ev, already shaped by
// FixtureShaper, as the adapter will identify it on channel's account.
type EventShaper interface {
	ShapeEvent(channel model.ChannelRef, ev InboundEvent) InboundEvent
}

// CredentialSecret is the secret every credential the suite's host
// resolves carries: a secret's bytes, a password's secret and an OAuth2
// access token. A WebhookProvider signs its requests with it, as the
// provider does with the account's own.
const CredentialSecret = adapterSecret

// FixtureShaper is implemented by a Provider whose IDs have a shape the
// suite's plain fixture values lack, such as email Message-IDs and
// addresses, or a destination only the account itself defines. The suite
// passes every fixture value through ShapeID, for the account it is used
// on, before using it: in InboundEvent fields, inbox destinations and the
// peer given to Route. Shaping must be deterministic and keep distinct
// values distinct.
type FixtureShaper interface {
	// ShapeID returns the provider form of fixture value id of kind on
	// channel's account.
	ShapeID(channel model.ChannelRef, kind FixtureKind, id string) string
}

// FixtureKind names what a fixture value identifies.
type FixtureKind string

// Fixture kinds.
const (
	// FixtureEvent is a provider event ID (InboundEvent.EventID).
	FixtureEvent FixtureKind = "event"
	// FixtureMessage is a provider message ID (InboundEvent.MessageID).
	FixtureMessage FixtureKind = "message"
	// FixtureSender is a remote party: an InboundEvent sender or claim and
	// the peer given to Route.
	FixtureSender FixtureKind = "sender"
	// FixtureDestination is a provider address an inbox binds.
	FixtureDestination FixtureKind = "destination"
	// FixtureRoom is a provider room or thread (InboundEvent.Room).
	FixtureRoom FixtureKind = "room"
)

// Account is the channel settings of one provider account.
type Account struct {
	Configuration model.Extension
	CredentialRef string
}

// InboundEvent is a provider event in provider terms. A harness renders it
// in the provider's own format; the adapter under test normalizes it.
type InboundEvent struct {
	// EventID and MessageID are the provider's event and message IDs. The
	// adapter must keep MessageID as the provider message reference.
	EventID   string
	MessageID string
	// Destination is the provider address the event was sent to.
	Destination string
	// Sender is the provider's authenticated ID of the author, already in
	// its normalized form: the adapter must keep it as the identity source
	// ID.
	Sender string
	// Room is the provider room or thread, for room- and thread-scoped
	// providers.
	Room string
	Text string
	// Visibility is the audience the provider gives the message; empty is
	// public.
	Visibility model.Visibility
	// Kind is what the event is; empty is a new message. An update kind
	// (edited, deleted, a reaction) names the existing provider message
	// MessageID, which the suite takes as the adapter reported it, and its
	// EventID is new. Text is an edit's whole new content and Emoji the
	// reaction of a reaction event; a removal may carry no Emoji, and a
	// provider that cannot express that is ErrNotExpressible.
	Kind  InboundKind
	Emoji string
	// Counts and Complete are a reaction summary's per-emoji totals and
	// whether the provider vouches they are whole. A summary has no
	// sender: it speaks for the provider.
	Counts   []model.ReactionCount
	Complete bool
	// SourceRevision is the provider's revision label of an update, and
	// OccurredAfter the time it happened after the instant new messages of
	// the suite carry; both order updates, and an update must be newer than
	// the version it follows to apply.
	SourceRevision string
	OccurredAfter  time.Duration
	// Interaction, when set, makes the event a structured interaction the
	// Sender (the actor) performed: EventID is the provider interaction ID,
	// MessageID the provider message the control sits on (empty when it
	// sits on none) and Text is unused. Only an adapter whose Events
	// declare interaction.received is judged on it; a provider that cannot
	// express it is ErrNotExpressible.
	Interaction *InboundInteraction
	// Echo, when set, makes the event the provider's echo of that attempt of
	// the suite's own mutation: the event carries model.EchoRef for it, as
	// the adapter reads it from the payload the provider round-tripped, and
	// reports it on the envelope.
	Echo *InboundEcho
	// Claims are unauthenticated identity claims the event carries in its
	// payload, such as display names, on-behalf-of fields or mentions. They
	// never identify the author.
	Claims []string
}

// InboundKind is the kind of an InboundEvent.
type InboundKind string

// Inbound event kinds.
const (
	// InboundMessage is a new message, the zero value.
	InboundMessage InboundKind = ""
	// InboundEdited replaces the content of an existing message.
	InboundEdited InboundKind = "edited"
	// InboundDeleted deletes an existing message.
	InboundDeleted InboundKind = "deleted"
	// InboundReactionAdded and InboundReactionRemoved add or remove the
	// sender's reaction Emoji on an existing message.
	InboundReactionAdded   InboundKind = "reaction_added"
	InboundReactionRemoved InboundKind = "reaction_removed"
	// InboundReactionSummary is the provider's per-emoji totals of an
	// existing message, without any reactor.
	InboundReactionSummary InboundKind = "reaction_summary"
)

// InboundInteraction is what a provider reports about an interaction.
type InboundInteraction struct {
	// Kind is a model.Interaction* kind; empty is an action.
	Kind string
	// Action, Command and Arguments are the provider identifiers and raw
	// argument text; Fields the submitted values under a named schema.
	Action, Command, Arguments string
	Fields                     model.Extension
	// Token is the provider's response credential, empty for none, and
	// TokenTTL how long after the instant new messages of the suite carry
	// it keeps working.
	Token    string
	TokenTTL time.Duration
	// Deadline is how long after that instant the provider needs its
	// protocol acknowledgement; a negative value is one that already
	// passed.
	Deadline time.Duration
}

// InboundEcho is the attempt of a mutation an echo event reports.
type InboundEcho struct {
	Operation model.OperationRef
	Attempt   model.ID
}

// InboundResult is one event the adapter handed to a sink. Acked reports
// whether the adapter acknowledged it to the provider; an unacknowledged
// event stays with the provider for redelivery.
type InboundResult struct {
	EventID string
	Acked   bool
}

// Outcome is a scripted provider answer to one outbound call.
type Outcome string

const (
	// OutcomeAccepted: the provider applied the effect and answered.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeRejected: the provider refused the effect; nothing applied.
	OutcomeRejected Outcome = "rejected"
	// OutcomeTransient: the call failed before reaching the provider;
	// nothing applied and a retry is safe.
	OutcomeTransient Outcome = "transient"
	// OutcomeRateLimited: the provider refused the call for now with
	// Response.RetryAfter; nothing applied.
	OutcomeRateLimited Outcome = "rate_limited"
	// OutcomeAnswerLost: the provider applied the effect but its answer
	// was lost; reconciliation finds the applied effect.
	OutcomeAnswerLost Outcome = "answer_lost"
	// OutcomeRequestLost: the request was lost before the provider applied
	// it and the adapter cannot tell; reconciliation finds no record.
	OutcomeRequestLost Outcome = "request_lost"
	// OutcomeUnauthorized: the provider refused a mutation by its own
	// ownership rule, such as an account editing a message it did not
	// write; nothing applied and a retry cannot help. A provider without
	// such a rule cannot script it.
	OutcomeUnauthorized Outcome = "unauthorized"
)

// Response is one scripted provider answer.
type Response struct {
	Outcome Outcome
	// RetryAfter is the wait an OutcomeRateLimited answer states.
	RetryAfter time.Duration
}

// ProviderCall is one outbound call as the provider observed it.
type ProviderCall struct {
	// Capability is the adapter capability the call served, such as
	// model.CapabilitySend or model.CapabilityReconcile.
	Capability model.Capability
	// Operation is the engine operation the call carried.
	Operation model.OperationRef
	// Route is where the provider delivered the effect.
	Route model.ExternalRef
	// Applied reports whether the provider carried out the effect.
	Applied bool
	// References are the provider references of an applied effect.
	References []model.ExternalRef
	// Audience is the audience the call carried.
	Audience model.Audience
	// Action is the extension action an extension_execute call carried.
	Action *model.Extension
	// Step is a stream call's step: StepStart, StepUpdate, StepFinish or
	// StepCancel. Sequence is an update's sequence.
	Step     string
	Sequence uint64
	// Content is the content the call carried, when it carried any.
	Content model.Content
	// Recipient is an ephemeral send's recipient.
	Recipient model.ExternalRef
	// Kind is a signal's kind, or a receipt's kind (model.ReceiptDelivered
	// or model.ReceiptRead).
	Kind string
	// Participant is the participant a membership call added or removed
	// (Remove), as the adapter reports that identity inbound: the
	// channel, the identity's namespace and source ID, kind
	// model.KindIdentity. Zero for other calls.
	Participant model.ExternalRef
	// Status and Reason are a work item act's status and status reason as
	// the call carried them, mapped back to the normalized status and
	// reason; Reason is empty when the call carried none.
	Status string
	Reason string
	// Attempt is the engine attempt the call carried.
	Attempt model.ID
	// Target is the provider message an edit, delete or reaction acted on;
	// zero for other calls.
	Target model.ExternalRef
	// Emoji and Remove are a reaction call's emoji and whether it removed
	// it; Remove is also whether a membership call removed its
	// participant.
	Emoji  string
	Remove bool
	// Mode and FollowUp are an interaction response's mode (a
	// model.ResponseMode* value) and whether it is a follow-up;
	// Interaction is the interaction it answered and Token the response
	// token the call carried. The suite never prints a call holding one.
	Mode        string
	FollowUp    bool
	Interaction model.InteractionRef
	Token       string
	// Items, Undo, Read, FlagKind, Destination, AddLabels and RemoveLabels
	// are a provider state call's request (capability archive, move,
	// read_state, flag or trash): the provider messages of the conversation
	// the call named, whether it undoes (unarchive, unflag, untrash), the
	// read value, the flag kind, and a move's destination and label
	// changes. Zero for other calls.
	Items        []model.ExternalRef
	Undo         bool
	Read         bool
	FlagKind     string
	Destination  model.MoveDestination
	AddLabels    []string
	RemoveLabels []string
}

// Stream call steps (ProviderCall.Step).
const (
	StepStart  = "start"
	StepUpdate = "update"
	StepFinish = "finish"
	StepCancel = "cancel"
)

type adapterCase struct {
	name string
	// needs lists capabilities the case requires; absent lists
	// capabilities whose declaration skips the case.
	needs  []model.Capability
	absent []model.Capability
	// anyOf skips the case unless at least one of its capabilities is
	// declared.
	anyOf []model.Capability
	// events lists event kinds the adapter's descriptor must declare; the
	// case skips otherwise.
	events []string
	// inbound cases need receive, or receive_http and a WebhookProvider.
	inbound bool
	run     func(*testing.T, *adapterEnv)
}

type adapterGroup struct {
	name  string
	cases []adapterCase
}

func adapterGroups() []adapterGroup {
	return []adapterGroup{
		{"Contract", slices.Concat(contractCases, discoveryCases)},
		{"Inbound", inboundCases},
		{"Outbound", slices.Concat(outboundCases, adapterStartCases, adapterRecipientsCases)},
		{"Extensions", slices.Concat(extensionCases, extensionEventCases)},
		{"Interactions", slices.Concat(interactionCases, respondCases)},
		{"InteractionIngress", interactionIngressCases},
		{"WorkItemActs", adapterWorkItemActCases},
		{"Mutations", slices.Concat(mutationCases, mutationInboundCases)},
		{"Receipts", adapterReceiptCases},
		{"Membership", adapterMembershipCases},
		{"History", slices.Concat(adapterHistoryCases, adapterHistoryMailCases)},
		{"State", slices.Concat(adapterStateCases, adapterStateInboundCases)},
		{"RawRead", adapterRawCases},
		{"Unsupported", slices.Concat(unsupportedCases, adapterRawUnsupportedCases, interactionUnsupportedCases, respondUnsupportedCases, adapterStartUnsupportedCases, adapterRecipientsUnsupportedCases, adapterWorkItemActUnsupportedCases, mutationUnsupportedCases, adapterStateUnsupportedCases)},
		{"PolicyBypass", slices.Concat(bypassCases, mutationBypassCases)},
	}
}
