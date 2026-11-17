package google

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
const Type = model.TypeGoogleMail

// IdentityNamespace is the namespace of sender identities: the From address
// of an inbound message with its domain lowercased and its local part kept
// byte-exact (model.NormalizeEmail).
const IdentityNamespace = string(Type) + ".address"

// refKindAddress is the ExternalRef kind of sender references and reply
// routes.
const refKindAddress = "address"

// Payload and part schemas this adapter emits.
const (
	// PayloadMessage is the envelope payload of an inbound message: Gmail
	// metadata that is not threading evidence (subject, threadId, labels,
	// recipients, Authentication-Results). Version 1.
	PayloadMessage = string(Type) + ".message"
	// PartAttachment is a structured content part describing one inbound
	// MIME part other than the body text (name, media type, size, SHA-256
	// digest). Version 1. Bytes are not kept.
	PartAttachment = string(Type) + ".attachment"
)

// ObservationSkipped is the observation kind reported for a message the
// receiver fetched and can never deliver: Gmail answered it whole, and its
// raw form is not base64url or does not parse as a message. Gmail never
// changes a message's bytes, so a fetch again fails alike: the receiver
// skips it and moves the cursor on. The receiver round that moves the
// cursor past it reports it, once per pass; a push request, which reads
// history without moving the cursor, does not. The observation names no
// message.
const ObservationSkipped = string(Type) + ".message_skipped"

// OAuth scopes. Reading the mailbox (history, messages.get, watch) needs
// gmail.readonly or gmail.modify, both restricted scopes that require
// Google's security assessment for public apps; sending needs gmail.send,
// a sensitive scope; changing a message's labels (messages.modify, the
// ExtensionLabels effect) and keeping a reply as a draft (users.drafts.*,
// the ExtensionReplyDraft effect) need gmail.modify, which alone also covers
// reading and sending. RequiredPermissions lists one scope per declared
// capability; a host that grants gmail.modify may leave the other two out,
// and one that grants only the other two gets unauthorized on every labels
// attempt (Gmail reveals a missing scope only on the call itself).
const (
	ScopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	ScopeSend     = "https://www.googleapis.com/auth/gmail.send"
	ScopeModify   = "https://www.googleapis.com/auth/gmail.modify"
)

// RequiredPermissions are the OAuth scopes the access token needs.
var RequiredPermissions = []string{ScopeReadonly, ScopeSend, ScopeModify}

// GoogleCertsURL is where Google publishes the keys that sign Pub/Sub push
// tokens (the jwks_uri of https://accounts.google.com).
const GoogleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

// Options configure a Factory. Gmail API calls go through the host's HTTP
// client, Dependencies.HTTP; Open fails without one.
type Options struct {
	// Endpoint overrides the Gmail API root,
	// "https://gmail.googleapis.com/gmail/v1". Tests point it at a local
	// server.
	Endpoint string
	// CertsURL overrides GoogleCertsURL.
	CertsURL string
	// PageSize is the history and message list page size; zero is 100.
	PageSize int
	// Jitter is the source of the +-10% spread the receiver puts on its
	// poll wait and its watch renewal time, so channels started together
	// do not stay in phase: uniform in [0, 1] (0 shortens by 10%, 0.5 keeps
	// the interval, 1 lengthens by 10%). Nil is the standard library's
	// generator. Tests and hosts set it to make the waits deterministic.
	Jitter func() float64

	// rawLimit overrides the raw message response bound, for tests.
	rawLimit int64
	// listMax overrides the page size of a history slice listing (500,
	// the most messages.list returns), for tests.
	listMax int
}

func (o Options) historyListMax() int {
	if o.listMax > 0 {
		return o.listMax
	}
	return historyListMax
}

// Factory is the google.mail adapter factory. It is safe for concurrent
// use and keeps, for the instances it opens in this process, per-channel
// receiver coordination (wake-ups from the push handler, receiver state
// when the engine supplies no protocol store) and the cache of Google's
// token signing keys.
type Factory struct {
	opts Options
	keys *keyCache

	mu       sync.Mutex
	channels map[model.ChannelRef]*channelState
}

// NewFactory returns a factory.
func NewFactory(opts Options) *Factory {
	if opts.PageSize <= 0 {
		opts.PageSize = 100
	}
	if opts.CertsURL == "" {
		opts.CertsURL = GoogleCertsURL
	}
	return &Factory{opts: opts, keys: &keyCache{url: opts.CertsURL}, channels: map[model.ChannelRef]*channelState{}}
}

// ReconcileSettle is the reconcile settle window the adapter declares:
// Gmail's search index, which rfc822msgid: queries, may show a sent
// message only some time after messages.send returned, so a search that
// finds nothing within it is inconclusive.
const ReconcileSettle = 2 * time.Minute

// capabilities: users.messages.send reaches any address, one no inbound
// mail named included, so a send can start a conversation.
var capabilities = []model.Capability{
	model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilityHistory, model.CapabilitySend, model.CapabilityReconcile,
	model.CapabilityStartConversation,
	model.CapabilityExtensionExecute,
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
	model.CapabilityReplyRecipients, model.CapabilityRawRead,
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
// mailboxes. Nothing else is folded: no plus-addressing, no Gmail dot rules,
// no punycode. An address that is not a bare addr-spec (a display name,
// angle brackets, a list, surrounding space) is refused. A reference of
// another namespace or kind is returned unchanged, for core to refuse.
func (f *Factory) NormalizeRecipient(recipient model.ExternalRef) (model.ExternalRef, error) {
	if recipient.Namespace != IdentityNamespace || recipient.Kind != refKindAddress {
		return recipient, nil
	}
	id, err := model.NormalizeEmail(recipient.ID)
	if err != nil {
		return model.ExternalRef{}, model.NewError(model.Validation, "must be a bare mailbox address")
	}
	recipient.ID = id
	return recipient, nil
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(e model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(e)
	return issues
}

// Open returns an instance for channel. It makes no Gmail call.
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
		return nil, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for the Gmail API", Retry: model.NoRetry}
	}
	endpoint := defaultEndpoint
	if f.opts.Endpoint != "" {
		endpoint = strings.TrimRight(f.opts.Endpoint, "/")
	}
	if u, err := url.Parse(endpoint); err != nil || u.Host == "" {
		return nil, &model.Error{Code: model.Permanent, SafeMessage: "invalid Gmail endpoint", Retry: model.NoRetry}
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock.Now
	}
	return &instance{
		f: f, ch: ch.Clone(), cfg: cfg, deps: deps, now: now, state: f.channel(ch.Ref), health: &pushHealth{}, link: &linkReport{},
		c: &client{
			http: httpc, base: endpoint + "/users/" + url.PathEscape(cfg.userID()), creds: deps.Credentials,
			channel: ch.Ref, credRef: ch.CredentialRef, now: now, rawLimit: f.opts.rawLimit,
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
