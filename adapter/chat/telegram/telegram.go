package telegram

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter type key.
const Type = model.TypeTelegramMessages

// Options configure a Factory. They are host settings, never channel
// configuration.
type Options struct {
	// PollInterval is the shortest time between two reads of getUpdates that
	// returned nothing; empty is DefaultPollInterval. A long poll that times
	// out empty has already taken Config.PollTimeoutSeconds, so this only
	// paces a server that answers at once.
	PollInterval time.Duration
	// Jitter is the source of the +-10% spread the receiver puts on that
	// pause, so channels started together do not stay in phase: uniform in
	// [0, 1] (0 shortens by 10%, 0.5 keeps the interval, 1 lengthens by
	// 10%). Nil is the standard library's generator. Tests and hosts set it
	// to make the pause deterministic.
	Jitter func() float64
}

// DefaultPollInterval is the pace of an idle receiver against a server that
// answers a long poll at once.
const DefaultPollInterval = time.Second

// Factory is the telegram.messages adapter.Factory. It is safe for
// concurrent use.
type Factory struct{ opts Options }

// NewFactory returns the adapter factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// Capabilities are the declared capabilities.
func Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityStartConversation,
		model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityRespond}
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
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider, Reactions: adapter.ReactionsSingle},
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
	x := &Instance{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, http: deps.HTTP, now: now, opts: f.opts, wait: sleep}
	if deps.Credentials != nil {
		v, err := newVerifier(x.channel, deps.Credentials, clockFunc(now))
		if err != nil {
			return nil, model.NewError(model.Validation, "webhook verification cannot be configured")
		}
		x.verifier = v
	}
	return x, nil
}

type clockFunc func() time.Time

func (c clockFunc) Now() time.Time { return c() }

// Instance is one open channel.
type Instance struct {
	channel  model.Channel
	cfg      Config
	creds    ports.Credentials
	http     ports.HTTPClient
	now      func() time.Time
	verifier *webhookauth.Verifier
	opts     Options
	// wait pauses the receiver, ending early when ctx does.
	wait func(ctx context.Context, d time.Duration) error
	// report is the supervisor's receiver link function, set before Run.
	report atomic.Pointer[func(bool)]

	mu     sync.Mutex
	closed bool
}

// ReportReceiver takes the function Run tells the host whether polling
// works through (adapter.ReceiverReporter).
func (x *Instance) ReportReceiver(report func(connected bool)) { x.report.Store(&report) }

// link tells the host polling works or not; it is advisory.
func (x *Instance) link(connected bool) {
	if f := x.report.Load(); f != nil {
		(*f)(connected)
	}
}

// Capabilities reports every declared capability available but the way of
// receiving Config.Receive withholds: receive_http under ReceivePoll and
// receive under ReceiveWebhook are unavailable, reason model.Disabled. The
// descriptor declares both for every channel; this report is what follows
// the channel's configuration, so the engine refuses Runtime.Receive on a
// webhook channel and Runtime.Webhook on a polling one before either runs.
func (x *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range Capabilities() {
		s := model.CapabilityStatus{Name: c, Available: x.cfg.offers(c)}
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

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
	_ adapter.Receiver     = (*Instance)(nil)

	_ adapter.ReceiverReporter = (*Instance)(nil)
)

// sleep waits d, or until ctx ends, whose error it returns.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
