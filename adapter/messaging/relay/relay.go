package relay

import (
	"context"
	"net/http"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Identity namespaces, one per medium: phone numbers exactly as the relay
// reports them. Reporting a number the relay normalized is the relay's job.
// An iMessage address is a phone number, kept the same way, or an email
// address, lowercased (see handleOf).
const (
	NamespaceSMS      = "relay.sms"
	NamespaceWhatsApp = "relay.whatsapp"
	NamespaceIMessage = "relay.imessage"
)

// RouteKind is the ExternalRef kind of a phone-number route.
const RouteKind = "phone"

// Credential purposes, resolved per request through the channel's credential
// port, never from the environment. PurposeWebhook is the shared secret (or,
// for ed25519, the public key) that authenticates the relay's webhooks: a
// ports.Secret, or a ports.Password whose secret is the key. PurposeCallback
// is the secret the adapter presents to the relay's callback: a ports.Secret.
const (
	PurposeWebhook  = "relay.webhook"
	PurposeCallback = "relay.callback"
)

// PayloadMedia names the structured part carrying an inbound message's media
// list. The adapter never fetches a URL it holds.
const PayloadMedia = "relay.messages.media"

// DefaultTimeout bounds one callback or lookup request.
const DefaultTimeout = 15 * time.Second

// ReconcileSettle is the least reconcile settle window the adapter
// declares: a lookup answering not_found within it of the attempt is
// inconclusive, since the callback may still be on its way to, or inside,
// the gateway. The window is twice Options.Timeout when that is longer.
const ReconcileSettle = 2 * time.Minute

// Options are the host's settings, fixed for every channel the factory
// opens. They are host settings, never channel configuration, so a channel
// cannot choose where the host's credentials are sent.
type Options struct {
	// CallbackHosts lists the hosts a channel's callback URL may name, each
	// exactly as it appears in the URL (host, or host:port). None: no
	// channel can send.
	CallbackHosts []string
	// AllowHTTP permits http callbacks, for local development. Credentials
	// then travel in clear text.
	AllowHTTP bool
	// Timeout bounds one callback or lookup request; zero is
	// DefaultTimeout.
	Timeout time.Duration
	// ReplayStore remembers accepted webhooks for channels that turn replay
	// rejection on. Nil: one in-memory store per factory, which a restart
	// forgets and replicas do not share; see docs/webhook-auth.md.
	ReplayStore webhookauth.ReplayStore
}

// Factory is the relay.messages adapter factory.
type Factory struct {
	opts Options

	replayOnce sync.Once
	replay     webhookauth.ReplayStore
}

// NewFactory returns the relay.messages factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// capabilities: send is a configured callback, and reaches any number the
// relay accepts, one no inbound message named included. reconcile is the
// optional lookup: declared for the type, available on a channel only when
// its callback names a lookup URL. receipt is not here: it is the adapter
// telling the sender of a message that it was delivered or read, and the
// relay contract has no such call. Delivery status the other way, the relay
// reporting what became of a reply, is the status route of receive_http
// (Config.Inbound.Status), a channel setting, not a capability.
var capabilities = []model.Capability{
	model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityStartConversation,
}

// Describe returns the descriptor. Message IDs are the sender's: the relay
// is a service the host's operator runs, and the adapter cannot promise its
// ids are unique, so core gives them no identity or echo meaning.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                model.TypeRelayMessages,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), capabilities...),
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{NamespaceSMS, NamespaceWhatsApp, NamespaceIMessage},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsSender},
		ReconcileSettle:     f.reconcileSettle(),
	}
}

// reconcileSettle is ReconcileSettle, or twice the request timeout when
// that is longer: a callback is never in flight past its timeout.
func (f *Factory) reconcileSettle() time.Duration {
	if d := 2 * f.opts.timeout(); d > ReconcileSettle {
		return d
	}
	return ReconcileSettle
}

// timeout bounds one request to the relay.
func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(c, f.opts)
	return issues
}

// NormalizeRecipient returns recipient with the identity spelling inbound
// reports (adapter.RecipientNormalizer): on iMessage an email address is
// lowercased, so a start to "Ann@Example.com" claims the identity an inbound
// from "ann@example.com" does. A phone number and every sms or whatsapp
// address are returned exactly as given. It never refuses: what cannot be an
// address is refused by a send, as before.
func (f *Factory) NormalizeRecipient(recipient model.ExternalRef) (model.ExternalRef, error) {
	recipient.ID = handleOf(recipient.Namespace, recipient.ID)
	return recipient, nil
}

// ReuseLegacyRecipient reports whether a start typed with recipient may reuse
// an identity stored under that exact spelling (adapter.LegacyRecipientReuser).
// Starts made before NormalizeRecipient claimed an iMessage email address as
// typed, and a conversation routed to it is still sent to, so only iMessage
// asks: every other address is never changed by NormalizeRecipient.
func (f *Factory) ReuseLegacyRecipient(recipient model.ExternalRef) bool {
	return recipient.Namespace == NamespaceIMessage
}

// replayStore is the store of channels that turn replay rejection on.
func (f *Factory) replayStore(clock ports.Clock) webhookauth.ReplayStore {
	if f.opts.ReplayStore != nil {
		return f.opts.ReplayStore
	}
	f.replayOnce.Do(func() { f.replay = webhookauth.NewMemoryReplayStore(0, clock) })
	return f.replay
}

// Open opens an instance for channel. It makes no network call.
func (f *Factory) Open(ctx context.Context, channel model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.Ref.Validate(); err != nil {
		return nil, err
	}
	if channel.Type != model.TypeRelayMessages {
		return nil, model.NewError(model.Validation, "channel type is not relay.messages")
	}
	cfg, issues := parseConfig(channel.Configuration, f.opts)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid relay.messages configuration")
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	i := &Instance{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: clock, opts: f.opts}
	switch cfg.Medium {
	case MediumWhatsApp:
		i.namespace = NamespaceWhatsApp
	case MediumIMessage:
		i.namespace = NamespaceIMessage
	default:
		i.namespace = NamespaceSMS
	}
	cfg.auth.MaxBodyBytes = cfg.maxBody
	reqs := []ports.CredentialRequest{{Channel: channel.Ref, Reference: channel.CredentialRef, Purpose: PurposeWebhook}}
	if cfg.previous != "" {
		reqs = append(reqs, ports.CredentialRequest{Channel: channel.Ref, Reference: cfg.previous, Purpose: PurposeWebhook})
	}
	var secrets webhookauth.SecretSource = unavailableSecrets{}
	if deps.Credentials != nil {
		secrets = webhookauth.FromCredentials(deps.Credentials, reqs...)
	}
	v, err := webhookauth.New(cfg.auth, secrets, webhookauth.Deps{
		Clock: clock, Replay: f.replayStore(clock), Scope: string(channel.Ref.Tenant) + "/" + string(channel.Ref.ID),
	})
	if err != nil {
		return nil, model.NewError(model.Validation, "invalid relay.messages authentication")
	}
	i.verifier = v
	if h := deps.HTTP; h != nil && cfg.Callback != nil {
		i.http = noRedirect(h)
	}
	return i, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// unavailableSecrets is the secret source of an instance opened without a
// credential port: every webhook then answers 503, never passes.
type unavailableSecrets struct{}

func (unavailableSecrets) Secrets(context.Context) ([][]byte, error) {
	return nil, model.NewError(model.Unavailable, "credentials port not configured")
}

// Instance is one open relay.messages channel.
type Instance struct {
	channel   model.Channel
	cfg       parsed
	opts      Options
	creds     ports.Credentials
	clock     ports.Clock
	namespace string
	verifier  *webhookauth.Verifier
	http      *http.Client

	mu     sync.Mutex
	closed bool
}

// Capabilities reports receive_http available while the instance is open.
// Send and start_conversation need a callback in the configuration and an
// HTTP client from the host: without a callback they are disabled, without a
// client unavailable. Reconcile needs the same and a lookup URL in the
// callback: without one it is disabled, and the engine leaves a lost answer
// outcome_unknown, never sent again.
func (i *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	open := !i.isClosed()
	out := make([]model.CapabilityStatus, 0, len(capabilities))
	for _, c := range capabilities {
		s := model.CapabilityStatus{Name: c, Available: open}
		switch {
		case !open:
			s.Reason = model.Unavailable
		case c == model.CapabilityReceiveHTTP:
		case i.cfg.Callback == nil:
			s.Available, s.Reason = false, model.Disabled
		case c == model.CapabilityReconcile && i.cfg.Callback.LookupURL == "":
			s.Available, s.Reason = false, model.Disabled
		case i.http == nil:
			s.Available, s.Reason = false, model.Unavailable
		}
		out = append(out, s)
	}
	return out, nil
}

// Close closes the instance; it is idempotent. A closed instance's handler
// answers 503 and its calls fail with model.Unavailable.
func (i *Instance) Close(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	return nil
}

func (i *Instance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

var (
	_ adapter.Factory               = (*Factory)(nil)
	_ adapter.RecipientNormalizer   = (*Factory)(nil)
	_ adapter.LegacyRecipientReuser = (*Factory)(nil)
)
