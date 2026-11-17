package microsoft

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Type is the adapter type key.
const Type = model.TypeMicrosoftMail

// IdentityNamespace is the namespace of sender identities: the SMTP address
// Graph reports in a message's from field, with its domain lowercased and
// its local part kept byte-exact (model.NormalizeEmail).
const IdentityNamespace = string(Type) + ".address"

// refKindAddress is the ExternalRef kind of sender references and reply
// routes.
const refKindAddress = "address"

// Payload and part schemas this adapter emits.
const (
	// PayloadMessage is the envelope payload of an inbound message: Graph
	// metadata that is not threading evidence (subject, conversationId,
	// recipients, selected headers). Version 1.
	PayloadMessage = string(Type) + ".message"
	// PartAttachment is a structured content part describing one inbound
	// attachment (Graph attachment ID, name, media type, size). Version 1.
	PartAttachment = string(Type) + ".attachment"
)

// RequiredPermissions are the Microsoft Graph permissions the adapter
// needs, delegated or application: Mail.ReadWrite (read, delta, change
// notifications, drafts, createReply, extended-property lookups, and the
// message update behind ExtensionCategories, the state verbs and the folder
// delta queries) and Mail.Send (send drafts).
var RequiredPermissions = []string{"Mail.ReadWrite", "Mail.Send"}

// Options configure a Factory. Graph calls go through the host's HTTP
// client, Dependencies.HTTP; Open fails without one.
type Options struct {
	// Endpoint overrides the configured cloud's Graph root, for example
	// "https://graph.microsoft.com/v1.0". Tests point it at a local server.
	Endpoint string
	// PageSize is the delta page size requested through
	// Prefer: odata.maxpagesize; zero is 50.
	PageSize int
	// Jitter is the source of the +-10% spread the receiver puts on its
	// poll wait and its subscription renewal margin, so channels started
	// together do not stay in phase: uniform in [0, 1] (0 shortens by 10%,
	// 0.5 keeps the interval, 1 lengthens by 10%). Nil is the standard
	// library's generator. Tests and hosts set it to make the waits
	// deterministic.
	Jitter func() float64
}

// Factory is the microsoft.mail adapter factory. It is safe for concurrent
// use and keeps per-channel receiver coordination (wake-ups from the
// webhook, delta state when the engine supplies no protocol store) for the
// instances it opens in this process.
type Factory struct {
	opts Options

	mu       sync.Mutex
	channels map[model.ChannelRef]*channelState
}

// NewFactory returns a factory.
func NewFactory(opts Options) *Factory {
	if opts.PageSize <= 0 {
		opts.PageSize = 50
	}
	return &Factory{opts: opts, channels: map[model.ChannelRef]*channelState{}}
}

// ReconcileSettle is the reconcile settle window the adapter declares:
// Graph documents that a message's Sent Items copy may appear only some
// time after the send, so a lookup that finds nothing within it is
// inconclusive.
const ReconcileSettle = 5 * time.Minute

// capabilities: a new draft reaches any address, one no inbound mail named
// included, so a send can start a conversation. History reads the inbox, the
// receive scope.
var capabilities = []model.Capability{
	model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile,
	model.CapabilityStartConversation,
	model.CapabilityExtensionExecute,
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
	model.CapabilityHistory, model.CapabilityReplyRecipients, model.CapabilityRawRead,
	model.CapabilityExtensionQuery, ExtensionMailboxes, ExtensionMailboxCreate,
}

// Describe returns the descriptor.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), capabilities...),
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{IdentityNamespace},
		RequiredPermissions: append([]string(nil), RequiredPermissions...),
		Maturity:            "experimental",
		ReconcileSettle:     ReconcileSettle,
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider, Threading: adapter.ThreadingEmail},
	}
}

// NormalizeRecipient returns recipient with the domain of its mailbox
// address lowercased (adapter.RecipientNormalizer), the spelling inbound
// keys a sender on: a start to "Bob@EXAMPLE.org" claims the identity of a
// message from "Bob@example.org". The local part stays byte-exact: RFC 5321
// reserves its case to the receiving host, so Bob@ and bob@ are two
// mailboxes. Nothing else is folded: no plus-addressing, no punycode. It
// never refuses: what is not a bare addr-spec is returned as given, as
// inbound keys such a sender, and a send decides on it as before (any plain
// token is a route; Graph answers for the rest). A reference of another
// namespace or kind is returned unchanged, for core to refuse.
func (f *Factory) NormalizeRecipient(recipient model.ExternalRef) (model.ExternalRef, error) {
	if recipient.Namespace == IdentityNamespace && recipient.Kind == refKindAddress {
		recipient.ID = identityAddress(recipient.ID)
	}
	return recipient, nil
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(e model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(e)
	return issues
}

// Open returns an instance for channel. It makes no Graph call.
func (f *Factory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ch.Ref.Validate(); err != nil {
		return nil, err
	}
	if ch.Type != Type {
		return nil, model.NewError(model.Validation, "channel type is not "+string(Type))
	}
	cfg, issues := parseConfig(ch.Configuration)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid "+ConfigSchema+" configuration")
	}
	httpc := deps.HTTP
	if httpc == nil {
		return nil, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for Microsoft Graph", Retry: model.NoRetry}
	}
	endpoint := strings.TrimRight(cfg.endpoint(), "/")
	if f.opts.Endpoint != "" {
		endpoint = strings.TrimRight(f.opts.Endpoint, "/")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Host == "" {
		return nil, &model.Error{Code: model.Permanent, SafeMessage: "invalid Graph endpoint", Retry: model.NoRetry}
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock.Now
	}
	return &instance{
		f: f, ch: ch.Clone(), cfg: cfg, deps: deps, now: now, state: f.channel(ch.Ref), health: &pushHealth{}, link: &linkReport{},
		c: &client{
			http: httpc, endpoint: endpoint, origin: origin, creds: deps.Credentials,
			channel: ch.Ref, credRef: ch.CredentialRef, now: now,
		},
	}, nil
}

// channel returns the process-local state of ch.
func (f *Factory) channel(ch model.ChannelRef) *channelState {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.channels[ch]
	if !ok {
		s = &channelState{wake: make(chan struct{}, 1)}
		f.channels[ch] = s
	}
	return s
}

var (
	_ adapter.Factory             = (*Factory)(nil)
	_ adapter.RecipientNormalizer = (*Factory)(nil)
)
