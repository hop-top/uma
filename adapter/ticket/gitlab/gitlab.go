package gitlab

import (
	"context"
	"net/http"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter type key.
const Type = model.TypeGitlabIssues

// Namespace is the namespace of every reference the adapter reports: users
// (kind RefKindUser, the numeric GitLab user ID) and issues (kind
// model.RefKindWorkItem, the issue's project-scoped number, iid, under the
// numeric project ID). It is also the identity namespace.
const Namespace = "gitlab.issues"

// RefKindUser is the reference kind of a GitLab user.
const RefKindUser = "user"

// Credential purposes; see the package documentation.
const (
	CredentialPurposeAPI     = "gitlab.issues.api"     // #nosec G101 -- purpose name, not a credential
	CredentialPurposeWebhook = "gitlab.issues.webhook" // #nosec G101 -- purpose name, not a credential
)

// Work item fields (model.WorkItemState.Fields), version 1. Labels data is
// the sorted array of label names.
const FieldLabels = "gitlab.issues.labels"

var capabilities = []model.Capability{
	model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityRefetch,
	model.CapabilityWorkItemAct, model.CapabilityEdit, model.CapabilityDelete, model.CapabilityHistory,
}

// workItemActs are the issue transitions: close from opened and reopen from
// closed. GitLab has no close reason, so none is offered.
func workItemActs() []adapter.WorkItemTransition {
	return []adapter.WorkItemTransition{
		{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}},
		{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}},
	}
}

// Options configure a Factory; there are none yet. Provider calls go
// through the host's HTTP client, adapter.Dependencies.HTTP; without it
// they fail with model.Unavailable.
type Options struct{}

// Factory is the gitlab.issues adapter.Factory. It is safe for concurrent
// use.
type Factory struct{ opts Options }

// NewFactory returns the adapter factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// Describe returns the descriptor: receive_http, send, edit, delete,
// history, reconcile, refetch and work_item_act with the issue transitions
// (close, reopen); bot senders.
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
		RequiredPermissions: []string{"api"},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
		SenderKinds:         []string{model.ContactKindBot},
		WorkItemActs:        workItemActs(),
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
	x := &Instance{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: deps.Clock}
	if h := deps.HTTP; h != nil {
		x.client = &http.Client{
			Transport:     doer{h},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return x, nil
}

// doer adapts the host's ports.HTTPClient to a RoundTripper.
type doer struct{ next ports.HTTPClient }

func (d doer) RoundTrip(r *http.Request) (*http.Response, error) { return d.next.Do(r) }

// Instance is one open channel. It implements adapter.HTTPReceiver,
// adapter.Sender, adapter.Editor, adapter.Deleter, adapter.Reconciler,
// adapter.Refetcher, adapter.WorkItemActor and adapter.HistoryReader.
type Instance struct {
	channel model.Channel
	cfg     Config
	creds   ports.Credentials
	clock   ports.Clock
	client  *http.Client

	mu     sync.Mutex
	closed bool
}

// Capabilities reports receive_http, refetch, work_item_act, reconcile and
// history available, and send, edit and delete available only when replies
// are enabled (reason disabled otherwise). Without replies, reconcile
// resolves work item acts only.
func (x *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	out := []model.CapabilityStatus{
		{Name: model.CapabilityReceiveHTTP, Available: true}, {Name: model.CapabilityRefetch, Available: true},
		{Name: model.CapabilityWorkItemAct, Available: true}, {Name: model.CapabilityReconcile, Available: true},
		{Name: model.CapabilityHistory, Available: true},
	}
	for _, c := range []model.Capability{model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete} {
		s := model.CapabilityStatus{Name: c, Available: x.cfg.Replies}
		if !x.cfg.Replies {
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

var (
	_ adapter.Factory       = (*Factory)(nil)
	_ adapter.HTTPReceiver  = (*Instance)(nil)
	_ adapter.Sender        = (*Instance)(nil)
	_ adapter.Editor        = (*Instance)(nil)
	_ adapter.Deleter       = (*Instance)(nil)
	_ adapter.Reconciler    = (*Instance)(nil)
	_ adapter.Refetcher     = (*Instance)(nil)
	_ adapter.WorkItemActor = (*Instance)(nil)
	_ adapter.HistoryReader = (*Instance)(nil)
)
