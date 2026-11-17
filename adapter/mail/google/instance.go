package google

import (
	"context"
	"errors"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/internal/unsubscribe"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// instance is one opened channel. It implements exactly the interfaces of
// the declared capabilities: Receiver, HTTPReceiver, Sender, Reconciler,
// ExtensionExecutor (labels.go), ExtensionReader (mailboxes.go), RawReader
// (raw.go) and the five state verbs (state_ops.go).
type instance struct {
	f     *Factory
	ch    model.Channel
	cfg   Config
	deps  adapter.Dependencies
	c     *client
	now   func() time.Time
	state *channelState

	// health and link report the push registration and the polling link to
	// the host (adapter.ReceiverDegradationReporter, ReceiverReporter).
	health *pushHealth
	link   *linkReport

	mu      sync.Mutex
	profile string // mailbox address read from the Gmail profile

	// names remembers label names by ID for state reports.
	names labelNames

	// unsub and unsubPost are the seams of the unsubscribe effect (public
	// host test, DKIM key resolver; resolver, dialer and roots of the
	// one-click request); zero in production.
	unsub     unsubscribe.Options
	unsubPost unsubscribe.Poster
}

var (
	_ adapter.Receiver     = (*instance)(nil)
	_ adapter.HTTPReceiver = (*instance)(nil)
	_ adapter.Sender       = (*instance)(nil)
	_ adapter.Reconciler   = (*instance)(nil)
	_ adapter.RawReader    = (*instance)(nil)

	_ adapter.ReceiverReporter            = (*instance)(nil)
	_ adapter.ReceiverDegradationReporter = (*instance)(nil)
)

// Capabilities reports every declared capability available, except
// receive_http on a channel without a Pub/Sub topic: no watch would ever
// publish to its push endpoint. A missing OAuth scope surfaces as
// model.Unauthorized on the call that needs it.
func (i *instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	out := make([]model.CapabilityStatus, 0, len(capabilities))
	for _, c := range capabilities {
		s := model.CapabilityStatus{Name: c, Available: true}
		if c == model.CapabilityReceiveHTTP && !i.cfg.push() {
			s.Available, s.Reason = false, model.Unsupported
		}
		if l, ok := stateLimits(c); ok {
			s.Limits = l
		}
		if c == model.CapabilityReplyRecipients {
			s.Limits = recipients.Limits().Extension()
		}
		out = append(out, s)
	}
	return out, nil
}

// Close releases nothing: an instance holds no connection or credential.
func (*instance) Close(context.Context) error { return nil }

// mailboxAddress is the mailbox's own address: Config.Address, else
// Config.User when it is an address, else the Gmail profile's
// emailAddress, read once.
func (i *instance) mailboxAddress(ctx context.Context, s *session) (string, error) {
	if i.cfg.Address != "" {
		return i.cfg.Address, nil
	}
	if i.cfg.User != "" && i.cfg.User != "me" {
		return i.cfg.User, nil
	}
	i.mu.Lock()
	p := i.profile
	i.mu.Unlock()
	if p != "" {
		return p, nil
	}
	prof, err := i.getProfile(ctx, s)
	if err != nil {
		return "", err
	}
	if !bareAddress(prof.EmailAddress) {
		return "", &model.Error{Code: model.Permanent, SafeMessage: "gmail profile without a usable address", Retry: model.NoRetry}
	}
	i.mu.Lock()
	i.profile = prof.EmailAddress
	i.mu.Unlock()
	return prof.EmailAddress, nil
}

func (i *instance) getProfile(ctx context.Context, s *session) (gProfile, error) {
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/profile", nil)})
	if err != nil {
		return gProfile{}, transient(err)
	}
	var p gProfile
	if err := decodeAnswer(resp.body, &p); err != nil {
		return gProfile{}, err
	}
	return p, nil
}

// observe reports an observation when the host supplied an observer.
func (i *instance) observe(ctx context.Context, kind string) {
	if i.deps.Observer != nil {
		i.deps.Observer.Observe(ctx, ports.Observation{Kind: kind, Adapter: Type})
	}
}

// isContext reports a context cancellation or deadline.
func isContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ReportReceiver takes the function Run tells the host whether polling
// works through (adapter.ReceiverReporter): connected once a round
// completed, whether or not push notifications work.
func (i *instance) ReportReceiver(report func(connected bool)) { i.link.bind(report) }

// ReportReceiverDegraded takes the function Run reports a failing push
// registration through (adapter.ReceiverDegradationReporter); see the
// Code constants.
func (i *instance) ReportReceiverDegraded(report func(code string)) { i.health.bind(report) }
