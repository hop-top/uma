package slack

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
const Type = model.TypeSlackChat

// Namespaces of the references the adapter reports. Namespace holds rooms
// (RefKindRoom, the conversation ID) and threads (RefKindThread, the
// thread's parent ts under its conversation ID); UserNamespace is the
// identity namespace: users and bots (RefKindUser, the user ID, or the
// bot ID of a bot that has no user), and the route of a direct message
// to a user.
const (
	Namespace     = "slack.chat"
	UserNamespace = "slack.chat.user"
	RefKindUser   = "user"
)

// Credential purposes; see the package documentation.
const (
	CredentialPurposeAPI     = "slack.chat.api"     // #nosec G101 -- purpose name, not a credential
	CredentialPurposeSigning = "slack.chat.signing" // #nosec G101 -- purpose name, not a credential
	// CredentialPurposeSocket is the app-level token (xapp-) Socket Mode
	// opens connections with. It needs the connections:write scope and is
	// only ever sent to apps.connections.open on the channel's api_url.
	CredentialPurposeSocket = "slack.chat.socket" // #nosec G101 -- purpose name, not a credential
)

// Options configure a Factory. They are host settings, never channel
// configuration: a channel cannot redirect a credential.
type Options struct {
	// ResponseBaseURL is the origin interaction response tokens resolve
	// against; empty is DefaultResponseBaseURL ("https://hooks.slack.com").
	// A response_url Slack reports is stored as its path alone and an
	// answer is only ever posted to this origin.
	ResponseBaseURL string
	// DebugReconnects appends debug_reconnects=true to every Socket Mode
	// connection URL, which makes Slack end a connection after 360 seconds
	// instead of hours: a way to watch the receiver refresh. Not for
	// production.
	DebugReconnects bool
}

// DefaultResponseBaseURL is the origin of Slack's response URLs.
const DefaultResponseBaseURL = "https://hooks.slack.com"

// Factory is the slack.chat adapter.Factory. It is safe for concurrent use.
type Factory struct{ opts Options }

// NewFactory returns the adapter factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// Capabilities are the declared capabilities.
func Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityStartConversation,
		model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityReconcile, model.CapabilityHistory,
		model.CapabilityRespond, model.CapabilityRespondFollowUp, model.CapabilityExtensionExecute, model.CapabilityArchive,
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
		RequiredPermissions: []string{"app_mentions:read", "channels:history", "channels:manage", "channels:read", "chat:write", "groups:history", "groups:read", "groups:write", "im:history", "im:write", "mpim:history", "reactions:read", "reactions:write"},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
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
	return &Instance{
		channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, http: deps.HTTP, observer: deps.Observer, now: now, opts: f.opts,
		sock: defaultSocketTiming, wait: sleep,
	}, nil
}

// Instance is one open channel.
type Instance struct {
	channel model.Channel
	cfg     Config
	creds   ports.Credentials
	http    ports.HTTPClient
	// observer receives the Socket Mode receiver's observations; nil drops
	// them.
	observer ports.Observer
	now      func() time.Time
	opts     Options
	// sock bounds Socket Mode connections; wait pauses the receiver,
	// ending early when ctx does.
	sock socketTiming
	wait func(ctx context.Context, d time.Duration) error
	// noMetadata is set once Slack refuses attempt metadata for good.
	noMetadata atomic.Bool
	// report is the supervisor's receiver link function, set before Run.
	report atomic.Pointer[func(bool)]

	mu     sync.Mutex
	closed bool
}

// ReportReceiver takes the function Run tells the host whether the Socket
// Mode link is up through (adapter.ReceiverReporter).
func (x *Instance) ReportReceiver(report func(connected bool)) { x.report.Store(&report) }

// link tells the host the Socket Mode link is up or not; it is advisory.
func (x *Instance) link(connected bool) {
	if f := x.report.Load(); f != nil {
		(*f)(connected)
	}
}

// Capabilities reports every declared capability available but the way of
// receiving Config.Receive withholds: receive under ReceiveEventsAPI and
// receive_http under ReceiveSocketMode are unavailable, reason
// model.Disabled. The descriptor declares both for every channel; this
// report is what follows the channel's configuration, so the engine refuses
// Runtime.Receive on a request URL channel and Runtime.Webhook on a Socket
// Mode one before either runs.
func (x *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range Capabilities() {
		s := model.CapabilityStatus{Name: c, Available: x.cfg.offers(c)}
		if c == model.CapabilityArchive {
			s.Limits = model.StateLimits{Scope: model.ScopeShared}.Extension()
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

// secret resolves the channel's credential for purpose as bytes.
func (x *Instance) secret(ctx context.Context, purpose string) ([]byte, error) {
	if x.creds == nil {
		return nil, &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: purpose,
		Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		return nil, err
	}
	s, ok := c.(ports.Secret)
	if !ok || len(s.Bytes) == 0 {
		return nil, &model.Error{Code: model.Unsupported, SafeMessage: "credential is not a secret", Retry: model.NoRetry}
	}
	return s.Bytes, nil
}

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
	_ adapter.Receiver     = (*Instance)(nil)

	_ adapter.ReceiverReporter = (*Instance)(nil)
)
