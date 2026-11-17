package jira

import (
	"context"
	"strings"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter type key.
const Type = model.TypeJiraIssues

// Namespace is the namespace of every reference the adapter reports: users
// (kind RefKindUser, the Atlassian accountId) and issues (kind
// model.RefKindWorkItem, the numeric issue ID). It is also the identity
// namespace.
const Namespace = "jira.issues"

// RefKindUser is the reference kind of a Jira user.
const RefKindUser = "user"

// Work item fields (model.WorkItemState.Fields), version 1. Labels data is
// the sorted array of label names.
const FieldLabels = "jira.issues.labels"

// EchoPropertyKey is the key of the comment property (entity property) the
// adapter sets on every comment it creates or edits: its value names the
// operation and attempt, so a lost answer is reconciled and an echo is
// recognised without any visible marker in the comment text.
const EchoPropertyKey = "uma.echo"

var capabilities = []model.Capability{
	model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityRefetch,
	model.CapabilityEdit, model.CapabilityDelete,
}

// Option configures a Factory.
type Option func(*Factory)

// WithBaseURL replaces the REST API root (the site URL, or the API gateway
// for OAuth 2.0 credentials) with base, such as "https://proxy.example.com".
// It serves proxies and test servers.
func WithBaseURL(base string) Option {
	return func(f *Factory) { f.base = strings.TrimRight(base, "/") }
}

// WithHTTPClient sets the HTTP client used when the engine injects none
// (adapter.Dependencies.HTTP is nil). Without either, provider calls fail
// with model.Unavailable.
func WithHTTPClient(c ports.HTTPClient) Option {
	return func(f *Factory) { f.http = c }
}

// Factory is the jira.issues adapter.Factory. It is safe for concurrent
// use.
type Factory struct {
	base string
	http ports.HTTPClient
}

// NewFactory returns the adapter factory.
func NewFactory(opts ...Option) *Factory {
	f := &Factory{}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Describe returns the descriptor: receive_http, send, edit, delete,
// reconcile and refetch; bot senders (Atlassian apps). Jira has no
// reactions, receipts or memberships, and no declared work item
// transitions: a transition is a workflow-specific ID. See the package
// documentation.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), capabilities...),
		Events: []string{
			model.EventMessageReceived, model.EventMessageUpdated,
			model.EventWorkItemUpdated, model.EventDeliveryUpdated,
		},
		IdentityNamespaces:  []string{Namespace},
		RequiredPermissions: []string{"read:jira-work", "write:jira-work"},
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
	client := deps.HTTP
	if client == nil {
		client = f.http
	}
	return &Instance{
		channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: deps.Clock,
		http: client, baseOverride: f.base,
	}, nil
}

// Instance is one open channel. It implements adapter.HTTPReceiver,
// adapter.Sender, adapter.Editor, adapter.Deleter, adapter.Reconciler and
// adapter.Refetcher.
type Instance struct {
	channel      model.Channel
	cfg          Config
	creds        ports.Credentials
	clock        ports.Clock
	http         ports.HTTPClient
	baseOverride string

	mu     sync.Mutex
	closed bool
}

// Capabilities reports receive_http, refetch and reconcile available (the
// last settles replies only once they are enabled), and send, edit and
// delete available only when replies are enabled (reason disabled
// otherwise).
func (x *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	out := []model.CapabilityStatus{
		{Name: model.CapabilityReceiveHTTP, Available: true},
		{Name: model.CapabilityRefetch, Available: true},
		{Name: model.CapabilityReconcile, Available: x.cfg.Replies},
	}
	for _, c := range []model.Capability{model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete} {
		out = append(out, model.CapabilityStatus{Name: c, Available: x.cfg.Replies})
	}
	if !x.cfg.Replies {
		for i := range out {
			if !out[i].Available {
				out[i].Reason = model.Disabled
			}
		}
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
	case x.clock == nil:
		return model.NewError(model.Unavailable, "no clock injected")
	case x.creds == nil:
		return &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	return nil
}

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
	_ adapter.Sender       = (*Instance)(nil)
	_ adapter.Editor       = (*Instance)(nil)
	_ adapter.Deleter      = (*Instance)(nil)
	_ adapter.Reconciler   = (*Instance)(nil)
	_ adapter.Refetcher    = (*Instance)(nil)
)
