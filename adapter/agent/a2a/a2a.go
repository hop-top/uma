package a2a

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter's type key.
const Type = model.TypeA2aMessages

// Channel configuration schema (see Config).
const (
	ConfigSchema         = string(Type) + ".config"
	ConfigVersion uint32 = 1
)

// Reference and structured-content names. NamespacePrincipal is the
// identity namespace of remote agents; NamespaceContext scopes the context
// references of conversations; SchemaData and SchemaFileURL name the
// structured parts that carry an A2A data part and a url part.
const (
	NamespacePrincipal = "a2a.principal"
	NamespaceContext   = "a2a.context"
	KindPrincipal      = "agent"
	SchemaData         = string(Type) + ".data"
	SchemaFileURL      = string(Type) + ".file_url"
)

// DefaultErrorDomain is the ErrorInfo domain of uma-defined error reasons
// when Options.ErrorDomain is empty.
const DefaultErrorDomain = "uma.hop.top"

// Defaults of the channel configuration.
const (
	DefaultBlockingWait = 30 * time.Second
	DefaultPollInterval = 250 * time.Millisecond
	minPollInterval     = 50 * time.Millisecond
)

// Principal is the verified remote agent a request came from.
type Principal struct {
	// ID is the agent's source ID in the NamespacePrincipal identity
	// namespace: 1 to 255 bytes of UTF-8 without control characters,
	// stable for the agent, never taken from the request body.
	ID string
}

// Authenticator is the host's authentication of an A2A request: it
// verifies the request (a bearer token, an API key, a client certificate,
// whatever the channel's declared security schemes say) and returns the
// agent it came from. An error refuses the request: a *model.Error with a
// retryable code answers 503, any other error 401. It runs before the
// request body is read. The adapter holds no credentials and verifies
// nothing itself.
type Authenticator func(*http.Request, model.ChannelRef) (Principal, error)

// Options configures a Factory. Authenticate is required.
type Options struct {
	Authenticate Authenticator
	// ErrorDomain is the ErrorInfo domain of uma-defined error reasons
	// (the blocking deadline, uma errors mapped to -32603): a stable,
	// deployment-owned namespace. Empty uses DefaultErrorDomain. It never
	// changes errors the A2A specification defines.
	ErrorDomain string
	// Jitter is the source of the +-10% spread a blocking SendMessage puts
	// on each wait between its store reads, so calls started together do
	// not read in phase: uniform in [0, 1] (0 shortens by 10%, 0.5 keeps
	// the interval, 1 lengthens by 10%). Nil is the standard library's
	// generator. Tests and hosts set it to make the waits deterministic.
	Jitter func() float64
}

// Factory is the adapter.Factory. The zero value is not usable; use New.
type Factory struct {
	authenticate Authenticator
	domain       string
	jitter       func() float64
}

// New returns a Factory. A nil Authenticate is a composition error.
func New(o Options) (*Factory, error) {
	if o.Authenticate == nil {
		return nil, model.NewError(model.Validation, "a2a: options.authenticate: required")
	}
	domain := o.ErrorDomain
	if domain == "" {
		domain = DefaultErrorDomain
	}
	if !validDomain(domain) {
		return nil, model.NewError(model.Validation, "a2a: options.error_domain: must be a DNS-style name")
	}
	return &Factory{authenticate: o.Authenticate, domain: domain, jitter: o.Jitter}, nil
}

func validDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	return !strings.HasPrefix(d, ".") && !strings.HasSuffix(d, ".")
}

var capabilities = []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend}

// Describe returns the descriptor: receive_http and send, messages
// received from remote agents, the identity namespace NamespacePrincipal
// and the sender kind agent. Message IDs are derived from the
// authenticated agent (provider authority).
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        slices.Clone(capabilities),
		Events:              []string{model.EventMessageReceived, model.EventMessageCreated, model.EventTaskUpdated},
		IdentityNamespaces:  []string{NamespacePrincipal},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
		SenderKinds:         []string{model.ContactKindAgent},
	}
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(c)
	return issues
}

// Open opens an instance bound to channel. It fails with model.Validation
// for a channel of another type or an invalid configuration.
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
	card, err := buildCard(cfg)
	if err != nil {
		return nil, err
	}
	return &Instance{f: f, ch: channel.Ref, cfg: cfg, clock: clock, card: card}, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

var _ ports.Clock = systemClock{}
var _ adapter.Factory = (*Factory)(nil)
