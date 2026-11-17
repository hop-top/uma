package zendesk

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter's type key.
const Type = model.TypeZendeskTickets

// Extension effects the adapter ships, registered from Factory.Extensions.
const (
	// ExtensionInternalNote posts an internal note (a private comment) on
	// the conversation's ticket. Its data is a Zendesk comment object
	// without "public", at least {"body": "..."} or {"html_body": "..."};
	// other comment fields pass through. The adapter always sends it with
	// "public": false and refuses a public audience. It is authorized as
	// model.ActionMessageNote under the request's audience.
	ExtensionInternalNote = string(Type) + ".internal_note"
	// ExtensionUpdate updates the conversation's ticket: its data is a
	// Zendesk ticket object (status, assignee_id, custom_fields, tags, ...)
	// passed through unchanged, without "comment" or "metadata": comments go
	// through replies and internal notes. It is authorized as
	// model.ActionWorkItemAct.
	ExtensionUpdate = string(Type) + ".update"
	// ExtensionVersion is the version of both effects.
	ExtensionVersion = 1
)

// capabilities are the declared capabilities, each served by the
// matching interface of *Instance.
var capabilities = []model.Capability{
	model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilitySend,
	model.CapabilityReconcile, model.CapabilityExtensionExecute, model.CapabilityInternalNote,
}

// Option configures a Factory.
type Option func(*Factory)

// WithBaseURL replaces the API origin https://{subdomain}.zendesk.com. The
// literal "{subdomain}" in base is replaced by the channel's subdomain. It
// serves proxies and test servers.
func WithBaseURL(base string) Option {
	return func(f *Factory) { f.base = strings.TrimRight(base, "/") }
}

// WithHTTPClient sets the HTTP client used when the engine injects none
// (adapter.Dependencies.HTTP is nil). Without either, http.DefaultClient.
func WithHTTPClient(c ports.HTTPClient) Option {
	return func(f *Factory) { f.http = c }
}

// WithJitter sets the source of the +-10% spread the polling receiver puts
// on its wait between polls, so channels started together do not stay in
// phase: uniform in [0, 1] (0 shortens by 10%, 0.5 keeps the interval, 1
// lengthens by 10%). The spread wait never falls below the 6 second minimum
// poll interval. Without it the standard library's generator is used. Tests
// and hosts set it to make the waits deterministic.
func WithJitter(source func() float64) Option {
	return func(f *Factory) { f.jitter = source }
}

// Factory is the zendesk.tickets adapter factory. It keeps no state across
// instances. It implements adapter.ActionValidator.
type Factory struct {
	base   string
	http   ports.HTTPClient
	jitter func() float64
}

// NewFactory returns the adapter factory.
func NewFactory(opts ...Option) *Factory {
	f := &Factory{base: "https://{subdomain}.zendesk.com"}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Describe returns the descriptor. Inbound authors are users in Namespace;
// tickets bind per-work-item conversations.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), capabilities...),
		Events: []string{
			model.EventMessageReceived, model.EventMessageUpdated, model.EventWorkItemUpdated, model.EventDeliveryUpdated,
		},
		IdentityNamespaces:  []string{Namespace},
		RequiredPermissions: []string{"agent", "admin:webhooks", "admin:incremental_export"},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}

// Extensions returns the extension effects the adapter ships, for the host
// to register with the factory.
func (f *Factory) Extensions() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{
		{
			Name: ExtensionInternalNote, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
			InputSchema:        model.Extension{Name: ExtensionInternalNote + ".input", Version: ExtensionVersion},
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionMessageNote,
		},
		{
			Name: ExtensionUpdate, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
			InputSchema:        model.Extension{Name: ExtensionUpdate + ".input", Version: ExtensionVersion},
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionWorkItemAct,
		},
	}
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := decodeConfig(c)
	return issues
}

// Open returns an instance bound to channel. It makes no provider call.
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
	cfg, issues := decodeConfig(channel.Configuration)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid zendesk channel configuration")
	}
	client := deps.HTTP
	if client == nil {
		client = f.http
	}
	if client == nil {
		client = http.DefaultClient
	}
	x := &instance{
		channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: deps.Clock, observer: deps.Observer,
		done: make(chan struct{}), jitter: f.jitter,
	}
	x.api = &api{
		base: strings.ReplaceAll(f.base, "{subdomain}", cfg.Subdomain), cfg: cfg, channel: x.channel,
		creds: deps.Credentials, http: client, clock: deps.Clock,
	}
	return &Instance{x: x}, nil
}

// instance is one opened channel.
type instance struct {
	channel  model.Channel
	cfg      Config
	creds    ports.Credentials
	clock    ports.Clock
	observer ports.Observer
	api      *api
	jitter   func() float64

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// Instance is an opened zendesk.tickets channel. It implements
// adapter.Receiver, adapter.HTTPReceiver, adapter.Sender,
// adapter.Reconciler and adapter.ExtensionExecutor.
type Instance struct{ x *instance }

// Capabilities reports every declared capability available: Zendesk
// reveals a withdrawn permission only by refusing the call itself.
func (i *Instance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.x.isClosed() {
		return nil, errClosed()
	}
	out := make([]model.CapabilityStatus, len(capabilities))
	for k, c := range capabilities {
		out[k] = model.CapabilityStatus{Name: c, Available: true}
	}
	return out, nil
}

// Close stops a running receiver; it is idempotent.
func (i *Instance) Close(context.Context) error {
	x := i.x
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.closed {
		x.closed = true
		close(x.done)
	}
	return nil
}

func (x *instance) isClosed() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.closed
}

func errClosed() error { return model.NewError(model.Unavailable, "zendesk instance is closed") }

var (
	_ adapter.Factory           = (*Factory)(nil)
	_ adapter.Receiver          = (*Instance)(nil)
	_ adapter.HTTPReceiver      = (*Instance)(nil)
	_ adapter.Sender            = (*Instance)(nil)
	_ adapter.Reconciler        = (*Instance)(nil)
	_ adapter.NoteSender        = (*Instance)(nil)
	_ adapter.ExtensionExecutor = (*Instance)(nil)
)
