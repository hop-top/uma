package discord

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter type key.
const Type = model.TypeDiscordChat

// Namespaces of the references the adapter reports. Namespace holds rooms
// (RefKindRoom, a channel ID) and threads (RefKindThread, a thread
// channel ID under its parent channel ID); UserNamespace is the identity
// namespace: users, bots and webhooks (RefKindUser, the author ID), and
// the route of a direct message to a user.
const (
	Namespace     = "discord.chat"
	UserNamespace = "discord.chat.user"
	RefKindUser   = "user"
)

// Credential purposes; see the package documentation.
const (
	// CredentialPurposeAPI is the bot token. It is only ever sent to the
	// channel's api_url and to the gateway in Identify and Resume.
	CredentialPurposeAPI = "discord.chat.api" // #nosec G101 -- purpose name, not a credential
	// CredentialPurposeInteractions is the application's public key, hex,
	// that verifies the Ed25519 signature of every request Discord sends to
	// the Interactions Endpoint URL.
	CredentialPurposeInteractions = "discord.chat.interactions" // #nosec G101 -- purpose name, not a credential
)

// Factory is the discord.chat adapter.Factory. It is safe for concurrent
// use.
type Factory struct{}

// NewFactory returns the adapter factory.
func NewFactory() *Factory { return &Factory{} }

// Capabilities are the declared capabilities.
func Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityStartConversation,
		model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityMembership, model.CapabilityHistory,
		model.CapabilityReconcile, model.CapabilityRefetch, model.CapabilitySendEphemeral, model.CapabilitySignal,
		model.CapabilityRespond, model.CapabilityRespondFollowUp, model.CapabilityArchive,
	}
}

// Describe returns the descriptor.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        Capabilities(),
		Events:              []string{model.EventMessageReceived, model.EventMessageUpdated, model.EventInteractionReceived},
		IdentityNamespaces:  []string{UserNamespace},
		RequiredPermissions: []string{"VIEW_CHANNEL", "SEND_MESSAGES", "SEND_MESSAGES_IN_THREADS", "READ_MESSAGE_HISTORY", "ADD_REACTIONS", "MANAGE_THREADS"},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider, Reactions: adapter.ReactionsMultiple},
		SenderKinds:         []string{model.ContactKindBot},
	}
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(c)
	return issues
}

// Open returns an instance bound to channel. It performs no I/O.
func (f *Factory) Open(ctx context.Context, channel model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.Ref.Validate(); err != nil {
		return nil, err
	}
	if channel.Type != Type {
		return nil, model.NewError(model.Validation, "channel type is not "+string(Type))
	}
	cfg, issues := parseConfig(channel.Configuration)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid channel configuration")
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock.Now
	}
	x := &Instance{
		channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, http: deps.HTTP, observer: deps.Observer, now: now,
		gw: defaultGatewayTiming, wait: sleep, jitter: randomJitter, threads: map[string]string{},
	}
	x.limits = newLimiter(x.nowTime, x.sleepFor)
	return x, nil
}

// Instance is one open channel.
type Instance struct {
	channel  model.Channel
	cfg      Config
	creds    ports.Credentials
	http     ports.HTTPClient
	observer ports.Observer
	now      func() time.Time
	// gw bounds gateway connections; wait pauses, ending early when ctx
	// does; jitter is the fraction of the heartbeat interval the first
	// heartbeat waits.
	gw     gatewayTiming
	wait   func(ctx context.Context, d time.Duration) error
	jitter func() float64
	limits *limiter
	// report is the supervisor's receiver link function, set before Run.
	report atomic.Pointer[func(bool)]
	// handled, when set, is told of the end of each gateway dispatch the
	// receiver handled: its sequence and the sink's refusal, nil when it
	// was recorded or not this channel's. Tests only.
	handled func(seq int64, refusal error)

	mu     sync.Mutex
	closed bool
	// threads maps a thread channel to its parent channel; a channel known
	// not to be a thread maps to "".
	threads map[string]string
}

func (x *Instance) nowTime() time.Time { return x.now() }

// ReportReceiver takes the function Run tells the host whether the gateway
// session is up through (adapter.ReceiverReporter).
func (x *Instance) ReportReceiver(report func(connected bool)) { x.report.Store(&report) }

// link tells the host the gateway session is up or not; it is advisory.
func (x *Instance) link(connected bool) {
	if f := x.report.Load(); f != nil {
		(*f)(connected)
	}
}

func (x *Instance) sleepFor(ctx context.Context, d time.Duration) error { return x.wait(ctx, d) }

// Capabilities reports every declared capability available but the way of
// receiving Config.Receive withholds: receive under ReceiveInteractions and
// receive_http under ReceiveGateway are unavailable, reason model.Disabled.
func (x *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range Capabilities() {
		s := model.CapabilityStatus{Name: c, Available: x.cfg.offers(c)}
		if c == model.CapabilityArchive {
			s.Limits = archiveLimits()
		}
		if !s.Available {
			s.Reason = model.Disabled
		}
		out = append(out, s)
	}
	return out, nil
}

// Close closes the instance; it is idempotent. A closed instance's handler
// answers 503 and its calls fail with model.Unavailable.
func (x *Instance) Close(context.Context) error {
	x.mu.Lock()
	x.closed = true
	x.mu.Unlock()
	return nil
}

func (x *Instance) isClosed() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.closed
}

// ready checks that the instance can call the provider.
func (x *Instance) ready() error {
	switch {
	case x.isClosed():
		return model.NewError(model.Unavailable, "instance closed")
	case x.http == nil:
		return model.NewError(model.Unavailable, "no HTTP client injected")
	case x.creds == nil:
		return &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	return nil
}

// token resolves the bot token for one call. It lives only in that call.
func (x *Instance) token(ctx context.Context, fresh bool) (string, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: []ports.CredentialKind{ports.KindSecret}, Fresh: fresh,
	})
	if err != nil {
		return "", err
	}
	s, ok := c.(ports.Secret)
	if !ok || len(s.Bytes) == 0 {
		return "", final(model.Unsupported, "credential is not a secret")
	}
	return string(s.Bytes), nil
}

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
	_ adapter.Receiver     = (*Instance)(nil)

	_ adapter.ReceiverReporter = (*Instance)(nil)
)
