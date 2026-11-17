package widget

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter's type key.
const Type = model.TypeNativeWebWidget

// Channel configuration schema. Its data is {"sites": ["<site>", ...]}:
// the sites (inbox destinations) visitors of the channel may be on.
const (
	ConfigSchema         = string(Type) + ".config"
	ConfigVersion uint32 = 1
)

// Reference namespaces and kinds. NamespaceVisitor and NamespaceUser are
// the identity namespaces of anonymous and verified visitors, whose sender
// references have kind KindVisitor and KindUser.
const (
	NamespaceVisitor = string(Type) + ".visitor"
	NamespaceUser    = string(Type) + ".user"
	KindVisitor      = "visitor"
	KindUser         = "user"
)

// Source assurance of a visitor (Visitor.Assurance, Envelope.Assurance).
const (
	AssuranceAnonymous = "anonymous"
	AssuranceVerified  = "verified"
)

// CredentialPurpose is the purpose under which the channel's session key is
// resolved: a ports.Secret of at least MinKeyBytes bytes, the SHA-256
// output length, so the key is never weaker than the MAC.
const (
	CredentialPurpose = string(Type) + ".session"
	MinKeyBytes       = 32
)

// Defaults for zero Options fields.
const (
	DefaultSessionTTL         = time.Hour
	DefaultHeartbeat          = 15 * time.Second
	DefaultInteractionTimeout = 10 * time.Second
)

// Visitor is who the host says a session request comes from. Assurance is
// AssuranceVerified for a host-authenticated user, whose ID is required,
// or AssuranceAnonymous; anonymous visitors without an ID get a random one,
// kept across token renewals. Site is the inbox destination the visitor is
// on; it may be empty when the channel lists one site.
type Visitor struct {
	ID        string
	Assurance string
	Site      string
}

// SessionVerifier establishes a visitor from a session request, for
// example from the host's own login cookie. An error refuses the session:
// a *model.Error with a retryable code answers 503, any other error 401.
type SessionVerifier interface {
	VerifySession(*http.Request, model.ChannelRef) (Visitor, error)
}

// VerifierFunc adapts a function to SessionVerifier.
type VerifierFunc func(*http.Request, model.ChannelRef) (Visitor, error)

// VerifySession calls f.
func (f VerifierFunc) VerifySession(r *http.Request, ch model.ChannelRef) (Visitor, error) {
	return f(r, ch)
}

// Options configures a Factory. Verifier is required.
type Options struct {
	Verifier SessionVerifier
	// Log keeps visitor logs; nil uses NewMemoryLog(0), which a restart
	// empties (see the package documentation, Durable delivery) and which
	// the host cannot reach to erase a visitor: pass your own MemoryLog or a
	// storelog.Log to have privacy erasure covered (see Log).
	Log Log
	// SessionTTL is the lifetime of an issued session token.
	SessionTTL time.Duration
	// Heartbeat is the idle interval after which an event stream sends a
	// comment, keeping proxies from closing it.
	Heartbeat time.Duration
	// InteractionTimeout bounds the synchronous evaluation of an
	// interaction submission and is the acknowledgement deadline of an
	// accepted action, command or submission.
	InteractionTimeout time.Duration
}

// Factory is the adapter.Factory. The host constructs it, registers it with
// the engine and mounts Events for each channel; Close ends every event
// stream. The zero value is not usable; use New.
type Factory struct {
	verifier           SessionVerifier
	log                Log
	ttl                time.Duration
	heartbeat          time.Duration
	interactionTimeout time.Duration

	mu       sync.Mutex
	closed   bool
	done     chan struct{}
	bindings map[model.ChannelRef]*binding
}

// binding is what the last opened instance of a channel knows: the
// channel, its parsed configuration, its credential port and clock. The
// event stream of the channel serves from it.
type binding struct {
	channel model.Channel
	cfg     config
	creds   ports.Credentials
	clock   ports.Clock
}

// New returns a Factory.
func New(o Options) (*Factory, error) {
	if o.Verifier == nil {
		return nil, model.NewError(model.Validation, "widget: options.verifier: required")
	}
	for _, d := range []time.Duration{o.SessionTTL, o.Heartbeat, o.InteractionTimeout} {
		if d < 0 {
			return nil, model.NewError(model.Validation, "widget: options: durations must not be negative")
		}
	}
	f := &Factory{
		verifier: o.Verifier, log: o.Log,
		ttl: o.SessionTTL, heartbeat: o.Heartbeat, interactionTimeout: o.InteractionTimeout,
		done: make(chan struct{}), bindings: map[model.ChannelRef]*binding{},
	}
	if f.log == nil {
		f.log = NewMemoryLog(0)
	}
	if f.ttl == 0 {
		f.ttl = DefaultSessionTTL
	}
	if f.heartbeat == 0 {
		f.heartbeat = DefaultHeartbeat
	}
	if f.interactionTimeout == 0 {
		f.interactionTimeout = DefaultInteractionTimeout
	}
	return f, nil
}

// capabilities: a send appends to the visitor's log whether or not the
// visitor wrote first, so a send can start a conversation; the visitor
// reads it on the next stream.
var capabilities = []model.Capability{
	model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityStartConversation, model.CapabilityStream,
	model.CapabilitySendEphemeral, model.CapabilitySignal,
	model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityReceipt,
	model.CapabilityRespond, model.CapabilityRespondFollowUp,
}

// Describe returns the descriptor: receive_http, send, start_conversation,
// stream, send_ephemeral, signal, edit, delete, react, receipt, respond and
// respond_follow_up, and the events message.received, delivery.updated and
// interaction.received.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        slices.Clone(capabilities),
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated, model.EventInteractionReceived},
		IdentityNamespaces:  []string{NamespaceVisitor, NamespaceUser},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}

// Configuration returns a channel configuration listing sites.
func Configuration(sites ...string) model.Extension {
	data, _ := json.Marshal(config{Sites: sites}) // #nosec G104 -- a string slice always marshals
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

type config struct {
	Sites []string `json:"sites"`
}

func (c config) site(s string) (string, bool) {
	if s == "" && len(c.Sites) == 1 {
		return c.Sites[0], true
	}
	return s, s != "" && slices.Contains(c.Sites, s)
}

// Validate checks a channel configuration without I/O: the schema, and at
// least one site, each a distinct non-empty string of at most 253 bytes
// without spaces or control characters.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(c)
	return issues
}

func parseConfig(c model.Extension) (config, []model.ValidationIssue) {
	issue := func(field, msg string) []model.ValidationIssue {
		return []model.ValidationIssue{{Field: field, Code: string(model.Validation), Message: msg}}
	}
	if c.Name != ConfigSchema || c.Version != ConfigVersion {
		return config{}, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch), Message: "want " + ConfigSchema + " version 1",
		}}
	}
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(c.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil || dec.More() {
		return config{}, issue("configuration.data", "must be an object with a sites list")
	}
	if len(cfg.Sites) == 0 {
		return config{}, issue("configuration.data.sites", "required")
	}
	for i, s := range cfg.Sites {
		if !validSite(s) || slices.Index(cfg.Sites, s) != i {
			return config{}, issue("configuration.data.sites", "sites must be distinct printable names")
		}
	}
	return cfg, nil
}

func validSite(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// Open opens an instance bound to channel. It fails with model.Validation
// for a channel of another type or an invalid configuration. The
// instance resolves the session key through deps.Credentials on every
// request and outbound call, and the channel's event stream serves from
// the latest opened instance's channel and credentials.
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
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	b := &binding{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: clock}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, model.NewError(model.Unavailable, "widget factory closed")
	}
	f.bindings[channel.Ref] = b
	return &Instance{f: f, b: b}, nil
}

// Close ends every open event stream; from then on Events answers 503 and
// Open fails with model.Unavailable. Instances already open keep working.
// It is idempotent and does not touch the engine.
func (f *Factory) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
}

func (f *Factory) binding(ch model.ChannelRef) (*binding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bindings[ch]
	return b, ok && !f.closed
}

// MessageRef is the provider message reference (model.ProviderMessageRef)
// of the widget message id on channel: what outbound operations report,
// what an edit, delete or reaction targets and what Event.Message names.
// The IDs of messages the widget sent and of those visitors sent share it.
func MessageRef(ch model.ChannelRef, id string) model.ExternalRef {
	return model.ProviderMessageRef(ch, id)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

var _ adapter.Factory = (*Factory)(nil)
