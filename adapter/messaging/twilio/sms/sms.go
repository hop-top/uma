// Package sms is the Twilio SMS adapter (type key twilio.sms).
//
// It declares send, reconcile, receive_http, start_conversation and
// history: a send reaches any number, so it can open a conversation
// (Conversations.Start), and the Messages list holds what the channel's
// number received, so a window of it can be imported (History).
// Outbound text goes through the Messages API on the HTTP client the host
// injects; incoming messages and status callbacks arrive on the handler of
// adapter.HTTPReceiver, which verifies X-Twilio-Signature before the sink
// sees anything. Credentials are resolved per request through the
// channel's credentials port, never from the environment.
//
// The Messages API has no idempotency key, so a lost answer or a 5xx after
// the request reached Twilio is outcome_unknown and the engine never
// resends it before Reconcile: it lists the account's messages for the one
// the attempt created, by route, sender, body and the attempt's record time
// (twilio.Client.FindSent). The list shows a new message only some time
// after creating it, so "not found" counts only after ReconcileSettle.
// Status callbacks name the attempt that created their message.
//
// An incoming MMS's media are downloaded from the account's Media
// resources with the REST credential (twilio.Client.Media), only from the
// API origin, and staged through the engine's attachment sink under their
// media SIDs; the message references the staged attachments next to the
// PayloadMedia summary.
package sms

import (
	"context"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Namespace is the identity and route namespace: phone numbers in E.164
// as Twilio reports them.
const Namespace = "twilio.sms"

// RouteKind is the ExternalRef kind of a phone-number route.
const RouteKind = "phone"

// Credential purposes. PurposeREST authenticates Messages API calls,
// preferring the auth token as ports.Secret (with the configured account
// SID; the channel needs that token anyway to verify webhooks), else a
// ports.Password of the account SID and auth token or of an API key SID
// (SK...) and secret. PurposeWebhook is the account's
// auth token, which signs webhooks: a ports.Secret, or a ports.Password
// whose Secret is the token.
const (
	PurposeREST    = "twilio.rest"
	PurposeWebhook = "twilio.webhook"
)

// Payload names of envelope payloads the adapter emits.
const (
	// PayloadDelivery is a status callback's delivery evidence.
	PayloadDelivery = "twilio.sms.delivery"
	// PayloadMedia lists the media an incoming message carried.
	PayloadMedia = "twilio.sms.media"
)

// ReconcileSettle is the reconcile settle window the adapter declares:
// Twilio's Messages list shows a new message only some time after creating
// it, so a listing that finds nothing within it is inconclusive.
const ReconcileSettle = 5 * time.Minute

// Options configure a Factory. REST calls go through the host's HTTP
// client, Dependencies.HTTP; without it Send fails as unavailable and
// webhooks still work.
type Options struct {
	// BaseURL overrides twilio.DefaultBaseURL, e.g. for a local test
	// server. It is a host setting, never channel configuration, so a
	// channel cannot redirect credentials elsewhere.
	BaseURL string
	// Timeout bounds one REST call; zero is twilio.DefaultTimeout.
	Timeout time.Duration
}

// Factory is the twilio.sms adapter factory.
type Factory struct{ opts Options }

// NewFactory returns the twilio.sms factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// capabilities: an SMS can be sent to any number, one no inbound message
// named included, so a send can start a conversation.
var capabilities = []model.Capability{
	model.CapabilitySend, model.CapabilityReconcile, model.CapabilityReceiveHTTP, model.CapabilityStartConversation,
	model.CapabilityHistory,
}

// Describe returns the descriptor.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                model.TypeTwilioSms,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), capabilities...),
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{Namespace},
		Maturity:            "experimental",
		ReconcileSettle:     ReconcileSettle,
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(c)
	return issues
}

// Open opens an instance for channel.
func (f *Factory) Open(ctx context.Context, channel model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.Ref.Validate(); err != nil {
		return nil, err
	}
	if channel.Type != model.TypeTwilioSms {
		return nil, model.NewError(model.Validation, "channel type is not twilio.sms")
	}
	cfg, issues := parseConfig(channel.Configuration)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid twilio.sms configuration")
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	i := &Instance{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: clock}
	if h := deps.HTTP; h != nil {
		c, err := twilio.NewClient(twilio.ClientConfig{HTTP: h, BaseURL: f.opts.BaseURL, Timeout: f.opts.Timeout, Clock: clock})
		if err != nil {
			return nil, model.NewError(model.Validation, "invalid Twilio client options")
		}
		i.rest = c
	}
	return i, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Instance is one open twilio.sms channel.
type Instance struct {
	channel model.Channel
	cfg     Config
	creds   ports.Credentials
	clock   ports.Clock
	rest    *twilio.Client

	mu     sync.Mutex
	closed bool
}

// Capabilities reports send, reconcile, receive_http, start_conversation
// and history. Twilio exposes no permission probe, so all report
// available while the instance is open, except history on a channel
// sending through a Messaging Service only: it has no number of its own
// to list (unsupported).
func (i *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	avail := !i.isClosed()
	out := make([]model.CapabilityStatus, 0, len(capabilities))
	for _, c := range capabilities {
		s := model.CapabilityStatus{Name: c, Available: avail}
		switch {
		case !avail:
			s.Reason = model.Unavailable
		case c == model.CapabilityHistory && i.cfg.From == "":
			s.Available, s.Reason = false, model.Unsupported
		}
		out = append(out, s)
	}
	return out, nil
}

// Close closes the instance; it is idempotent.
func (i *Instance) Close(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	return nil
}

func (i *Instance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

// resolve resolves the channel credential for purpose.
func (i *Instance) resolve(ctx context.Context, purpose string, accept ...ports.CredentialKind) (ports.Credential, error) {
	if i.creds == nil {
		return nil, &model.Error{Code: model.Unavailable, SafeMessage: "credentials port not configured", Retry: model.NoRetry}
	}
	return i.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: i.channel.Ref, Reference: i.channel.CredentialRef, Purpose: purpose, Accept: accept,
	})
}

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.Sender       = (*Instance)(nil)
	_ adapter.Reconciler   = (*Instance)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
)
