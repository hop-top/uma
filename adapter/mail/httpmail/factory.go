package httpmail

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
)

// Type is the adapter type key.
const Type = model.TypeHttpMail

// Options are the host-supplied parts of the adapter.
type Options struct {
	// ReplayStore remembers accepted deliveries for a channel whose
	// authentication turns replay rejection on (Config.Auth.Replay). Nil
	// refuses such a configuration. The bundled
	// webhookauth.MemoryReplayStore forgets on restart; see Webhook
	// authentication in the documentation.
	ReplayStore webhookauth.ReplayStore
}

// Factory opens http.mail instances.
type Factory struct{ opts Options }

// NewFactory returns the http.mail factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// Describe declares receive_http alone: the adapter takes mail a relay posts
// and has no way back to the sender.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP},
		IdentityNamespaces:  []string{Namespace},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsSender, Threading: adapter.ThreadingEmail},
	}
}

// Validate checks a channel configuration: its shape, and that the host can
// serve what it asks (a replay store for replay rejection).
func (f *Factory) Validate(x model.Extension) []model.ValidationIssue {
	cfg, issues := ParseConfig(x)
	if len(issues) > 0 {
		return issues
	}
	return f.hostIssues(cfg)
}

func (f *Factory) hostIssues(cfg Config) []model.ValidationIssue {
	if cfg.Auth.Replay != nil && f.opts.ReplayStore == nil {
		return []model.ValidationIssue{issue("auth.replay", "needs the host's Options.ReplayStore")}
	}
	return nil
}

// Open opens an instance for channel. Nothing is read until a request
// arrives.
func (f *Factory) Open(_ context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if ch.Type != Type {
		return nil, model.NewError(model.Validation, "channel type is not "+string(Type))
	}
	cfg, issues := ParseConfig(ch.Configuration)
	if issues = append(issues, f.hostIssues(cfg)...); len(issues) > 0 {
		return nil, &model.Error{Code: model.Validation, SafeMessage: "invalid configuration: " + issues[0].Field, Retry: model.NoRetry}
	}
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	i := &Instance{ch: ch, cfg: cfg, deps: deps}
	v, err := webhookauth.New(cfg.webhookConfig(), i.secrets(), webhookauth.Deps{
		Clock: deps.Clock, Replay: f.opts.ReplayStore,
		Scope: string(ch.Ref.Tenant) + "/" + string(ch.Ref.ID),
	})
	if err != nil {
		return nil, &model.Error{Code: model.Validation, SafeMessage: "invalid configuration: auth", Retry: model.NoRetry}
	}
	i.verifier = v
	return i, nil
}

// Instance is one opened http.mail channel.
type Instance struct {
	ch       model.Channel
	cfg      Config
	deps     adapter.Dependencies
	verifier *webhookauth.Verifier
	closed   atomic.Bool
}

// secrets resolves the request credential through the host's port on every
// request: a rotated credential applies at once, and nothing is kept.
func (i *Instance) secrets() webhookauth.SecretSource {
	if i.deps.Credentials == nil {
		return webhookauth.SecretFunc(func(context.Context) ([][]byte, error) {
			return nil, fmt.Errorf("%w: no credential port", webhookauth.ErrSecret)
		})
	}
	return webhookauth.FromCredentials(i.deps.Credentials, i.credentialRequest())
}

// Capabilities reports receive_http, available once the host supplies a
// credential port: without one no request can be authenticated.
func (i *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	st := model.CapabilityStatus{Name: model.CapabilityReceiveHTTP, Available: i.deps.Credentials != nil}
	if !st.Available {
		st.Reason = model.Unavailable
	}
	return []model.CapabilityStatus{st}, nil
}

// Close marks the instance closed; it is idempotent.
func (i *Instance) Close(context.Context) error {
	i.closed.Store(true)
	return nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
)
