package microsoft

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/internal/unsubscribe"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// instance is one opened channel. It implements exactly the interfaces of
// the declared capabilities: Receiver, HTTPReceiver, Sender, Reconciler
// ExtensionExecutor (categories.go) and the state verbs (stateverbs.go).
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
// receive_http on a channel without a notification URL: no subscription
// would ever deliver to its webhook. A revoked Graph permission surfaces
// as model.Unauthorized on the call that needs it; Graph offers no
// documented way to check it beforehand. A channel in SendModeDirect holds
// Mail.Send alone: it reports every capability but send and
// start_conversation unavailable (unsupported).
func (i *instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	declared := capabilities
	if i.f != nil && i.f.opts.SendOnly {
		declared = sendOnlyCapabilities
	}
	out := make([]model.CapabilityStatus, 0, len(declared))
	for _, c := range declared {
		s := model.CapabilityStatus{Name: c, Available: true, Limits: stateLimits(c)}
		if c == model.CapabilityReceiveHTTP && i.cfg.NotificationURL == "" {
			s.Available, s.Reason = false, model.Unsupported
		}
		if c == model.CapabilityReplyRecipients {
			s.Limits = recipients.Limits().Extension()
		}
		if i.cfg.sendMode() == SendModeDirect && !slices.Contains(sendOnlyCapabilities, c) {
			// Direct mode holds Mail.Send alone: nothing here reads the
			// mailbox, replies in a thread or looks a sent copy up.
			s.Available, s.Reason, s.Limits = false, model.Unsupported, model.Extension{}
		}
		out = append(out, s)
	}
	return out, nil
}

// Close releases nothing: an instance holds no connection or credential.
func (*instance) Close(context.Context) error { return nil }

// clientState returns the subscription client state: an HMAC of the
// channel reference under the host's notification secret, so one secret
// can serve several channels without one channel accepting another's
// notifications, and the secret itself never reaches Graph. It is 43
// characters, under Graph's 128-character limit.
func (i *instance) clientState(ctx context.Context) (string, error) {
	if i.deps.Credentials == nil {
		return "", &model.Error{Code: model.Unauthorized, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	cred, err := i.deps.Credentials.Resolve(ctx, ports.CredentialRequest{
		Channel: i.ch.Ref, Reference: i.ch.CredentialRef, Purpose: PurposeNotifications,
		Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		return "", credentialError(ctx, err)
	}
	var key []byte
	switch v := cred.(type) {
	case ports.Secret:
		key = v.Bytes
	case *ports.Secret:
		if v != nil {
			key = v.Bytes
		}
	default:
		return "", &model.Error{Code: model.Unsupported, SafeMessage: "notification client state needs a secret credential", Retry: model.NoRetry}
	}
	if len(key) == 0 {
		return "", &model.Error{Code: model.Unauthorized, SafeMessage: "empty notification secret", Retry: model.NoRetry}
	}
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(string(Type) + " client state\x00" + string(i.ch.Ref.Tenant) + "\x00" + string(i.ch.Ref.ID)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
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
