// Package engine is the composed core every engine concern shares: a
// validated composition, the adapter and extension registry, lifecycle,
// actor-bound client binding and the host-owned ports. Services, ingress,
// operations and the runtime build on it; it imports none of them.
package engine

import (
	"cmp"
	"context"
	"mime"
	"slices"
	"strings"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Config is an engine composition. Its fields mirror the public
// composition one for one; the public New converts.
type Config struct {
	Store                store.Store
	Authorizer           policy.Authorizer
	IdentityVerifiers    map[string]policy.IdentityVerifier
	Selectors            map[string]policy.ConversationSelector
	InteractionValidator policy.InteractionValidator
	CascadePolicy        policy.CascadePolicy
	Adapters             []adapter.Factory
	Extensions           []adapter.ExtensionDefinition
	Credentials          ports.Credentials
	HTTP                 ports.HTTPClient
	Clock                ports.Clock
	IDs                  ports.IDs
	Blobs                ports.BlobStore
	Observer             ports.Observer
	Waiter               ports.Waiter
	Retry                RetryPolicy
	Retention            RetentionPolicy
	Work                 *WorkPolicy
	RequireDurable       bool
	WebhookBodyLimit     int64
	AttachmentSizeLimit  int64
	AttachmentMediaTypes []string
	Held                 HeldPolicy
}

// HeldPolicy is Config.Held: the local writes a plugin actor's hold may be
// applied to, beyond the ones always holdable. Every field defaults to
// false, and a hold on a write not opted in is model.Unsupported, claiming
// nothing and applying nothing.
type HeldPolicy struct {
	// LabelConfigure opts Labels.Configure, which creates or updates a
	// label definition, into holding.
	LabelConfigure bool
	// LabelDelete opts Labels.Delete into holding.
	LabelDelete bool
	// ConversationTransition opts Conversations.Transition into holding.
	ConversationTransition bool
	// StartCreateAtHold sets when a held Conversations.Start creates its
	// conversation. False, the default: when the start is approved, so a
	// rejected or expired proposal leaves no conversation and the
	// approval's selection policy sees the conversations as they are then.
	// True: in the unit that holds the start, which names the conversation
	// and its message at hold time; a conversation a rejected or expired
	// start opened stays, empty.
	StartCreateAtHold bool
}

// DefaultWebhookBodyLimit is the webhook body limit when Config leaves
// WebhookBodyLimit zero: 1 MiB.
const DefaultWebhookBodyLimit int64 = 1 << 20

// DefaultAttachmentSizeLimit is the attachment size limit when Config
// leaves AttachmentSizeLimit zero: 25 MiB.
const DefaultAttachmentSizeLimit int64 = 25 << 20

// New composes an engine from cfg. It validates the composition only: it
// performs no I/O, starts no goroutines, opens no adapter instance and
// consults no policy. A rejected composition returns a *model.Error with
// code model.Validation naming the Config field.
//
// Registration rejects, with no override and no last-wins rule: a nil
// factory; a descriptor type failing model.ValidateTypeKey (grammar,
// reserved-unimplemented maintained keys, reserved prefixes) or registered
// twice; a contract major other than adapter.ContractMajor; a capability
// that is neither builtin nor a namespaced name some extension definition
// requires; respond_follow_up declared without respond; an event that is
// neither builtin nor a namespaced kind with an event-kind definition;
// duplicate capabilities or events; a configuration
// schema registered by two adapters or named like an extension definition;
// and an extension definition with an invalid or reserved name, zero
// version, unknown kind, missing or invalid policy action or capability,
// invalid schema names, a duplicate name and version, a kind or policy
// action other than another version of its name has, or an effect
// authorizing as a builtin read-only or core-only action (see
// adapter.ExtensionDefinition).
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.Store == nil:
		return nil, ConfigError("Store", "required")
	case cfg.Authorizer == nil:
		return nil, ConfigError("Authorizer", "required")
	case cfg.Clock == nil:
		return nil, ConfigError("Clock", "required")
	case cfg.IDs == nil:
		return nil, ConfigError("IDs", "required")
	}
	if err := FirstErr(cfg.Retry.validate(), cfg.Retention.validate()); err != nil {
		return nil, err
	}
	work, err := workPolicy(cfg.Work)
	if err != nil {
		return nil, err
	}
	if cfg.WebhookBodyLimit < 0 {
		return nil, ConfigError("WebhookBodyLimit", "must not be negative")
	}
	if cfg.AttachmentSizeLimit < 0 {
		return nil, ConfigError("AttachmentSizeLimit", "must not be negative")
	}
	mediaTypes, err := mediaTypeList(cfg.AttachmentMediaTypes)
	if err != nil {
		return nil, err
	}
	if cfg.RequireDurable && !cfg.Store.Properties().Durable {
		return nil, ConfigError("Store", "durable store required; backend is not durable")
	}
	verifiers, err := copyPolicies("IdentityVerifiers", cfg.IdentityVerifiers)
	if err != nil {
		return nil, err
	}
	for k := range verifiers {
		if !model.IsEvidenceKind(k) {
			return nil, ConfigError("IdentityVerifiers", "key is not an evidence kind")
		}
	}
	selectors, err := copyPolicies("Selectors", cfg.Selectors)
	if err != nil {
		return nil, err
	}
	reg, err := newRegistry(cfg.Adapters, cfg.Extensions)
	if err != nil {
		return nil, err
	}
	commits := &eventCommits{}
	return &Engine{
		store:       signalingStore{Store: cfg.Store, commits: commits},
		commits:     commits,
		authorizer:  cfg.Authorizer,
		verifiers:   verifiers,
		selectors:   selectors,
		interaction: cfg.InteractionValidator,
		cascade:     cfg.CascadePolicy,
		reg:         reg,
		retry:       cfg.Retry.withDefaults(),
		retention:   cfg.Retention,
		work:        work,
		held:        cfg.Held,

		webhookBodyLimit: cmp.Or(cfg.WebhookBodyLimit, DefaultWebhookBodyLimit),
		attachmentLimit:  cmp.Or(cfg.AttachmentSizeLimit, DefaultAttachmentSizeLimit),
		mediaTypes:       mediaTypes,
		deps: dependencies{
			credentials: cfg.Credentials,
			http:        cfg.HTTP,
			clock:       cfg.Clock,
			ids:         cfg.IDs,
			blobs:       cfg.Blobs,
			observer:    cfg.Observer,
			waiter:      cfg.Waiter,
		},
	}, nil
}

// mediaTypeList validates and lowercases the accepted attachment media
// types: each a type/subtype or type/* without parameters, distinct.
func mediaTypeList(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, t := range in {
		mt, params, err := mime.ParseMediaType(t)
		typ, sub, ok := strings.Cut(mt, "/")
		if err != nil || len(params) != 0 || !ok || typ == "" || typ == "*" || sub == "" || strings.Contains(sub, "/") {
			return nil, ConfigError("AttachmentMediaTypes", "not a type/subtype or type/* without parameters")
		}
		if slices.Contains(out, mt) {
			return nil, ConfigError("AttachmentMediaTypes", "duplicate")
		}
		out = append(out, mt)
	}
	return out, nil
}

// copyPolicies copies a caller-owned policy map, rejecting empty keys and
// nil values.
func copyPolicies[V any](field string, in map[string]V) (map[string]V, error) {
	out := make(map[string]V, len(in))
	for k, v := range in {
		if k == "" {
			return nil, ConfigError(field, "empty key")
		}
		if any(v) == nil {
			return nil, ConfigError(field, "nil value")
		}
		out[k] = v
	}
	return out, nil
}

// Engine is one composed engine. Its registrations and ports never change
// after New.
type Engine struct {
	store       store.Store
	commits     *eventCommits
	authorizer  policy.Authorizer
	verifiers   map[string]policy.IdentityVerifier
	selectors   map[string]policy.ConversationSelector
	interaction policy.InteractionValidator
	cascade     policy.CascadePolicy
	reg         *registry
	retry       RetryPolicy
	retention   RetentionPolicy
	work        WorkPolicy
	held        HeldPolicy
	deps        dependencies

	webhookBodyLimit int64
	attachmentLimit  int64
	mediaTypes       []string

	mu       sync.Mutex
	closed   bool
	active   int
	drained  chan struct{} // made by the first Close; closed when active reaches 0
	stoppers []func()
}

// dependencies are the host-owned infrastructure ports. The engine never
// closes them.
type dependencies struct {
	credentials ports.Credentials
	http        ports.HTTPClient
	clock       ports.Clock
	ids         ports.IDs
	blobs       ports.BlobStore
	observer    ports.Observer
	waiter      ports.Waiter
}

// Store returns the host's store, signalling EventCommits.
func (e *Engine) Store() store.Store { return e.store }

// Clock returns the host's time source.
func (e *Engine) Clock() ports.Clock { return e.deps.clock }

// IDs returns the host's internal ID generator.
func (e *Engine) IDs() ports.IDs { return e.deps.ids }

// Credentials returns the host's credential port, possibly nil.
func (e *Engine) Credentials() ports.Credentials { return e.deps.credentials }

// HTTP returns the host's HTTP client, possibly nil.
func (e *Engine) HTTP() ports.HTTPClient { return e.deps.http }

// Observer returns the host's observer, possibly nil.
func (e *Engine) Observer() ports.Observer { return e.deps.observer }

// Waiter returns the host's waiter, possibly nil.
func (e *Engine) Waiter() ports.Waiter { return e.deps.waiter }

// Retry returns the retry policy with defaults filled.
func (e *Engine) Retry() RetryPolicy { return e.retry }

// WebhookBodyLimit returns the webhook body limit in bytes, default filled.
func (e *Engine) WebhookBodyLimit() int64 { return e.webhookBodyLimit }

// Held returns the policy of the local writes that may be held.
func (e *Engine) Held() HeldPolicy { return e.held }

// Blobs returns the host's attachment blob store, possibly nil.
func (e *Engine) Blobs() ports.BlobStore { return e.deps.blobs }

// AttachmentSizeLimit returns the attachment size limit in bytes, default
// filled.
func (e *Engine) AttachmentSizeLimit() int64 { return e.attachmentLimit }

// AttachmentMediaTypes returns the accepted attachment media types,
// lowercased, as type/subtype or type/*; empty accepts any.
func (e *Engine) AttachmentMediaTypes() []string { return slices.Clone(e.mediaTypes) }

// For returns a client bound to p. The host must supply p from trusted
// authentication. The client holds references, not grants: every operation
// consults the current policy and resource state.
func (e *Engine) For(p model.Principal) (*Client, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if e.IsClosed() {
		return nil, ErrClosed()
	}
	return &Client{e: e, principal: p}, nil
}

// Bind returns a client for p without validating it or checking Close: the
// engine's own path for acting as a principal it already trusts, such as
// the originating actor of a stored operation.
func (e *Engine) Bind(p model.Principal) *Client { return &Client{e: e, principal: p} }

// Describe returns copies of the registered adapter descriptors in
// registration order. It performs no I/O.
func (e *Engine) Describe() []adapter.Descriptor { return e.reg.describe() }

// ValidateChannel checks a channel configuration for a registered adapter
// type without I/O: the configuration schema name must be a valid namespaced
// name matching the descriptor's schema, then the factory validates a copy.
func (e *Engine) ValidateChannel(t model.TypeKey, cfg model.Extension) []model.ValidationIssue {
	reg, ok := e.reg.lookup(t)
	if !ok {
		return []model.ValidationIssue{{
			Field: "type", Code: string(model.Unsupported), Message: "adapter type not registered",
		}}
	}
	if err := model.ValidateExtensionName(cfg.Name); err != nil {
		return []model.ValidationIssue{{
			Field: "configuration.name", Code: string(model.Validation), Message: SafeMessage(err),
		}}
	}
	if s := reg.desc.ConfigurationSchema; s.Name != "" && (cfg.Name != s.Name || cfg.Version != s.Version) {
		return []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch),
			Message: "configuration schema does not match the adapter descriptor",
		}}
	}
	return reg.factory.Validate(cfg.Clone())
}

// OnStop registers fn to run once when Close starts, after new work is
// refused and before Close waits for in-flight work. Concerns holding
// engine-scoped state (runs, bindings) register while the engine is
// composed, before it is used.
func (e *Engine) OnStop(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stoppers = append(e.stoppers, fn)
}

// Close stops new work and waits for in-flight work until ctx ends, then
// returns ctx's error. It is idempotent: a later call waits again and
// returns nil once in-flight work has finished. Close never closes
// host-owned values from Config: store, policies, factories or ports.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		e.drained = make(chan struct{})
		if e.active == 0 {
			close(e.drained)
		}
	}
	drained := e.drained
	stoppers := e.stoppers
	e.mu.Unlock()
	for _, stop := range stoppers {
		stop()
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Begin admits one unit of engine work, or fails once Close has started.
// The returned func ends the work and must be called exactly once.
func (e *Engine) Begin() (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed()
	}
	e.active++
	var once sync.Once
	return func() { once.Do(e.end) }, nil
}

func (e *Engine) end() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active--
	if e.closed && e.active == 0 {
		close(e.drained)
	}
}

// IsClosed reports whether Close has started.
func (e *Engine) IsClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// IdentityVerifier returns the verifier registered under key. An
// unregistered key is unsupported; there is no default verifier.
func (e *Engine) IdentityVerifier(key string) (policy.IdentityVerifier, error) {
	if v, ok := e.verifiers[key]; ok {
		return v, nil
	}
	return nil, model.NewError(model.Unsupported, "identity verifier not registered")
}

// ConversationSelector returns the selector registered under name. A
// missing selector denies.
func (e *Engine) ConversationSelector(name string) (policy.ConversationSelector, error) {
	if s, ok := e.selectors[name]; ok {
		return s, nil
	}
	return nil, model.NewError(model.Unauthorized, "no conversation selector configured; denied")
}

// HasSelector reports whether a selector is registered under name.
func (e *Engine) HasSelector(name string) bool {
	_, ok := e.selectors[name]
	return ok
}

// InteractionValidator returns the configured validator. A missing
// validator denies.
func (e *Engine) InteractionValidator() (policy.InteractionValidator, error) {
	if e.interaction == nil {
		return nil, model.NewError(model.Unauthorized, "no interaction validator configured; denied")
	}
	return e.interaction, nil
}

// ContactCascadeThreshold returns the contact-cascade threshold of inbox
// for p (see policy.CascadePolicy): policy.CascadeNeverRefuse when no
// policy is configured. A policy error is mapped as an Authorizer error
// is; a value below -1 is model.Permanent.
func (e *Engine) ContactCascadeThreshold(ctx context.Context, p model.Principal, inbox model.InboxRef) (int, error) {
	if e.cascade == nil {
		return policy.CascadeNeverRefuse, nil
	}
	n, err := e.cascade.ContactCascadeThreshold(ctx, p, inbox)
	if err != nil {
		return 0, policyError(ctx, err)
	}
	if n < policy.CascadeNeverRefuse {
		return 0, &model.Error{Code: model.Permanent, SafeMessage: "cascade threshold out of range", Retry: model.NoRetry}
	}
	return n, nil
}

// ErrClosed reports work refused because Close has started. Retrying on the
// same engine cannot succeed.
func ErrClosed() error {
	return &model.Error{Code: model.Unavailable, SafeMessage: "engine closed", Retry: model.NoRetry}
}

// Unsupported reports an operation not implemented in this version.
func Unsupported(op string) error {
	return model.NewError(model.Unsupported, op+": not implemented in this version")
}

// Unsupported reports op as not implemented, or the closed-engine error once
// Close has started.
func (e *Engine) Unsupported(op string) error {
	if e.IsClosed() {
		return ErrClosed()
	}
	return Unsupported(op)
}

// FirstErr returns the first non-nil error.
func FirstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
