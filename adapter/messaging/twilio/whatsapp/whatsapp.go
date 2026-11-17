// Package whatsapp is the Twilio WhatsApp adapter (type key
// twilio.whatsapp). One channel is one WhatsApp sender on a Twilio account,
// or a Messaging Service holding WhatsApp senders.
//
// It declares send, reconcile and receive_http, on the same Programmable Messaging API
// and X-Twilio-Signature webhooks as twilio.sms; Twilio addresses WhatsApp
// users and senders as "whatsapp:" plus the number, or plus the user's
// business-scoped user ID (CC.<id>) when WhatsApp hides the number. Routes
// and identities carry the address without that prefix. An incoming message's media are
// downloaded with the REST credential and staged through the engine's
// attachment sink, as for twilio.sms.
//
// WhatsApp allows free-form messages only inside the 24-hour customer
// service window a user's message opens; outside it only an approved
// template may be sent. Send takes either text parts (free-form, Body) or
// one TemplateSchema part (ContentSid and ContentVariables). When the host
// gives the instance protocol state, verified inbound messages record each
// user's window and a free-form message after a recorded window closed is
// rejected before any call; without that state, or for a user with no
// recorded inbound message, Twilio decides (error 63016).
//
// The Messages API has no idempotency key, so a lost answer or a 5xx after
// the request reached Twilio is outcome_unknown and the engine never
// resends it before Reconcile: it lists the account's messages for the
// free-form one the attempt created, by route, sender, body and the
// attempt's record time (twilio.Client.FindSent); a template message is
// not matched. The list shows a new message only some time after creating
// it, so "not found" counts only after ReconcileSettle. Every send tags its status callback URL with the operation
// and attempt, and the status envelope names that attempt.
package whatsapp

import (
	"context"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Namespace is the identity and route namespace: WhatsApp addresses as
// Twilio reports them, without the "whatsapp:" prefix: an E.164 number, or
// a business-scoped user ID (CC.<id>) for a user whose number WhatsApp
// hides. Inbound.ExternalUserID carries the ID either way.
const Namespace = "twilio.whatsapp"

// RouteKind is the ExternalRef kind of a WhatsApp user route.
const RouteKind = "user"

// AddressPrefix is Twilio's channel prefix on WhatsApp addresses.
const AddressPrefix = "whatsapp:"

// Credential purposes, as for twilio.sms. PurposeREST authenticates
// Messages API calls, preferring the auth token as ports.Secret (with the
// configured account SID), else a ports.Password of the account SID and
// auth token or of an API key SID (SK...) and secret. PurposeWebhook is the
// account's auth token, which signs webhooks: a ports.Secret, or a
// ports.Password whose Secret is the token.
const (
	PurposeREST    = "twilio.rest"
	PurposeWebhook = "twilio.webhook"
)

// Schemas.
const (
	// ConfigSchema is the channel configuration: Config as JSON.
	ConfigSchema  = "twilio.whatsapp.config"
	ConfigVersion = 1
	// TemplateSchema is an outbound template message: Template as JSON.
	TemplateSchema  = "twilio.whatsapp.template"
	TemplateVersion = 1
	// PayloadInbound is the payload of inbound message envelopes: Inbound.
	PayloadInbound = "twilio.whatsapp.inbound"
	// PayloadDelivery is a status callback's delivery evidence: Delivery.
	PayloadDelivery = "twilio.whatsapp.delivery"
	// PayloadMedia lists the media an incoming message carried: Media.
	PayloadMedia = "twilio.whatsapp.media"
	// PayloadButton is a quick-reply button press: Button.
	PayloadButton = "twilio.whatsapp.button"
	// PayloadLocation is a shared location: Location.
	PayloadLocation = "twilio.whatsapp.location"
	// WindowSchema is the protocol state recording a user's last verified
	// inbound message.
	WindowSchema  = "twilio.whatsapp.window"
	WindowVersion = 1
)

// ServiceWindow is the customer service window a user's message opens.
const ServiceWindow = 24 * time.Hour

// CodeOutsideWindow is Twilio's error for a free-form message outside the
// customer service window.
const CodeOutsideWindow = 63016

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

// Factory is the twilio.whatsapp adapter factory.
type Factory struct{ opts Options }

// NewFactory returns the twilio.whatsapp factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// Capabilities are the declared capabilities. A send reaches any WhatsApp
// user, one no inbound message named included, so a send can start a
// conversation; outside the user's service window it must carry a
// template part, which WhatsApp enforces.
func Capabilities() []model.Capability {
	return []model.Capability{model.CapabilitySend, model.CapabilityReconcile, model.CapabilityReceiveHTTP, model.CapabilityStartConversation}
}

// Describe returns the descriptor.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                model.TypeTwilioWhatsapp,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        Capabilities(),
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

// Open opens an instance for channel. It makes no network call.
func (f *Factory) Open(ctx context.Context, channel model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.Ref.Validate(); err != nil {
		return nil, err
	}
	if channel.Type != model.TypeTwilioWhatsapp {
		return nil, model.NewError(model.Validation, "channel type is not twilio.whatsapp")
	}
	cfg, issues := parseConfig(channel.Configuration)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid twilio.whatsapp configuration")
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	i := &Instance{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, clock: clock, protocol: deps.Protocol}
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

// Instance is one open twilio.whatsapp channel.
type Instance struct {
	channel  model.Channel
	cfg      Config
	creds    ports.Credentials
	clock    ports.Clock
	protocol adapter.ProtocolState
	rest     *twilio.Client

	mu     sync.Mutex
	closed bool
}

// Capabilities reports send, reconcile and receive_http. Twilio exposes no
// permission probe, so all report available while the instance is open.
func (i *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	avail := !i.isClosed()
	caps := Capabilities()
	out := make([]model.CapabilityStatus, 0, len(caps))
	for _, c := range caps {
		s := model.CapabilityStatus{Name: c, Available: avail}
		if !avail {
			s.Reason = model.Unavailable
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
