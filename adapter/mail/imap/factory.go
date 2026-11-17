package imap

import (
	"context"
	"crypto/x509"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// accept are the credential kinds both protocols can use, preferred first.
var accept = []ports.CredentialKind{ports.KindOAuth2, ports.KindPassword, ports.KindClientCert}

// Options are the host-supplied parts of the adapter.
type Options struct {
	// RootCAs verifies the IMAP and submission server certificates; nil
	// uses the system roots.
	RootCAs *x509.CertPool
	// ReconnectBackoff is the first wait after a failed connection; it
	// doubles per consecutive failure up to a minute. Zero is one second.
	ReconnectBackoff time.Duration
	// Waiter paces the receiver's timed waits: the IDLE restart, the poll
	// interval, the wait until a session is replaced and the reconnect
	// backoff. It must return when its context ends. Nil uses timers.
	Waiter ports.Waiter
	// Jitter is the source of the +-10% spread the receiver puts on its IDLE
	// restart and its poll interval, so channels started together do not
	// stay in phase: uniform in [0, 1] (0 shortens by 10%, 0.5 keeps the
	// interval, 1 lengthens by 10%). An IDLE restart is never spread past
	// MaxIdleRestart. Nil is the standard library's generator. Tests and
	// hosts set it to make the waits deterministic.
	Jitter func() float64
}

// Factory opens imap.mail instances. It keeps the receiver cursor of the
// channels it opened, for engines without a protocol store.
type Factory struct {
	opts Options

	mu    sync.Mutex
	state map[model.ChannelRef]*memState
	// info is what the sessions of each channel learned about its server.
	info infoCache
}

// NewFactory returns the imap.mail factory.
func NewFactory(opts Options) *Factory {
	if opts.ReconnectBackoff <= 0 {
		opts.ReconnectBackoff = time.Second
	}
	return &Factory{opts: opts, state: map[model.ChannelRef]*memState{}}
}

// capabilities: submission reaches any address, one no inbound mail named
// included, so a send can start a conversation. The state capabilities are
// the mailbox verbs of the account: archive, move, read_state, flag and
// trash.
var capabilities = []model.Capability{
	model.CapabilityReceive, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityStartConversation,
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
	model.CapabilityHistory, model.CapabilityReplyRecipients, model.CapabilityRawRead,
	model.CapabilityExtensionQuery, model.CapabilityExtensionExecute, ExtensionMailboxes, ExtensionMailboxCreate,
}

// Describe declares receive, send, reconcile, start_conversation, history,
// reply_recipients, raw_read, the provider state verbs and the mailbox list
// and create extensions.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                Type,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), capabilities...),
		IdentityNamespaces:  []string{Namespace},
		RequiredPermissions: []string{PurposeIMAP, PurposeSubmission},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsSender, Threading: adapter.ThreadingEmail},
	}
}

// NormalizeRecipient returns recipient with the domain of its mailbox
// address lowercased (adapter.RecipientNormalizer), the spelling inbound
// reports: a start to "Bob@EXAMPLE.org" claims the identity of a message
// from "Bob@example.org", and is sent to the route a send accepts. The local
// part stays byte-exact: RFC 5321 reserves its case to the receiving host, so
// Bob@ and bob@ are two mailboxes. Nothing else is folded: no
// plus-addressing, no punycode. An address that is not a bare addr-spec (a
// display name, angle brackets, a list, surrounding space) is refused. A
// reference of another namespace or kind is returned unchanged, for core to
// refuse.
func (f *Factory) NormalizeRecipient(recipient model.ExternalRef) (model.ExternalRef, error) {
	if recipient.Namespace != Namespace || recipient.Kind != RouteKind {
		return recipient, nil
	}
	id, err := model.NormalizeEmail(recipient.ID)
	if err != nil {
		return model.ExternalRef{}, model.NewError(model.Validation, "must be a bare mailbox address")
	}
	recipient.ID = id
	return recipient, nil
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(x model.Extension) []model.ValidationIssue {
	_, issues := ParseConfig(x)
	return issues
}

// Open opens an instance for channel. Nothing connects until Run, Send or
// Reconcile.
func (f *Factory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ch.Type != Type {
		return nil, model.NewError(model.Validation, "channel type is not "+string(Type))
	}
	cfg, issues := ParseConfig(ch.Configuration)
	if len(issues) > 0 {
		return nil, &model.Error{Code: model.Validation, SafeMessage: "invalid configuration: " + issues[0].Field, Retry: model.NoRetry}
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock.Now
	}
	return &Instance{f: f, ch: ch.Clone(), cfg: cfg, deps: deps, now: now, mem: f.memState(ch.Ref)}, nil
}

func (i *Instance) waiter() ports.Waiter {
	if w := i.f.opts.Waiter; w != nil {
		return w
	}
	return timerWaiter{}
}

// timerWaiter waits on a timer.
type timerWaiter struct{}

func (timerWaiter) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (f *Factory) memState(ch model.ChannelRef) *memState {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[ch]
	if !ok {
		s = &memState{}
		f.state[ch] = s
	}
	return s
}

// Instance is one opened imap.mail channel.
type Instance struct {
	f      *Factory
	ch     model.Channel
	cfg    Config
	deps   adapter.Dependencies
	now    func() time.Time
	mem    *memState
	closed atomic.Bool
	// report is the supervisor's receiver link function, set before Run.
	report atomic.Pointer[func(bool)]
}

// ReportReceiver takes the function Run tells the host whether an
// authenticated session stands through (adapter.ReceiverReporter).
func (i *Instance) ReportReceiver(report func(connected bool)) { i.report.Store(&report) }

// link tells the host a session stands or not; it is advisory.
func (i *Instance) link(connected bool) {
	if f := i.report.Load(); f != nil {
		(*f)(connected)
	}
}

var (
	_ adapter.Factory             = (*Factory)(nil)
	_ adapter.RecipientNormalizer = (*Factory)(nil)
	_ adapter.Receiver            = (*Instance)(nil)
	_ adapter.ReceiverReporter    = (*Instance)(nil)
	_ adapter.Sender              = (*Instance)(nil)
	_ adapter.Reconciler          = (*Instance)(nil)
	_ adapter.Archiver            = (*Instance)(nil)
	_ adapter.Mover               = (*Instance)(nil)
	_ adapter.ReadStateSetter     = (*Instance)(nil)
	_ adapter.Flagger             = (*Instance)(nil)
	_ adapter.Trasher             = (*Instance)(nil)
	_ adapter.HistoryReader       = (*Instance)(nil)
)

// Capabilities reports every capability available with a credentials port,
// and reconcile only with a sent-copies mailbox: it covers lost sends and
// lost state changes alike, and without a mailbox to look sent copies up in
// the host should not run reconcile passes at all. A state change whose
// answer was lost then settles from the receiver's report of the change (see
// state_sync.go). The state verbs are reported with their limits (see
// stateStatuses) and are available only when the server offers what each
// needs.
func (i *Instance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	out := make([]model.CapabilityStatus, 0, len(capabilities))
	var state map[model.Capability]model.CapabilityStatus
	for _, c := range capabilities {
		s := model.CapabilityStatus{Name: c, Available: true}
		switch {
		case i.deps.Credentials == nil:
			s.Available, s.Reason = false, model.Unavailable
		case c == model.CapabilityReconcile && i.cfg.Sent.Mailbox == "":
			s.Available, s.Reason = false, model.Unsupported
		case isStateCapability(c):
			if state == nil {
				state = i.stateStatuses(ctx)
			}
			s = state[c]
		}
		if c == model.CapabilityReplyRecipients {
			s.Limits = recipients.Limits().Extension()
		}
		out = append(out, s)
	}
	return out, nil
}

// Close marks the instance closed; it is idempotent. A running receiver
// stops with its context.
func (i *Instance) Close(context.Context) error {
	i.closed.Store(true)
	return nil
}

func (i *Instance) errClosed() error {
	return &model.Error{Code: model.Unavailable, SafeMessage: "imap.mail: instance closed", Retry: model.NoRetry}
}

func (i *Instance) noCredentials(purpose string) error {
	return &model.Error{Code: model.Unavailable, SafeMessage: purpose + ": credentials port not configured", Retry: model.NoRetry}
}

// observe reports an observation when the host supplied an observer.
func (i *Instance) observe(ctx context.Context, kind string) {
	if i.deps.Observer != nil {
		i.deps.Observer.Observe(ctx, ports.Observation{Kind: kind, Adapter: Type})
	}
}
