// Package whatsapp is the WhatsApp Cloud API adapter (type key
// meta.whatsapp). One channel is one business phone number.
//
// Capabilities are receive_http, send and react:
//
//   - The webhook handler answers Meta's subscription verification (GET)
//     and verifies the X-Hub-Signature-256 of every event notification
//     (POST) over the raw body before anything reaches the sink. It takes
//     only changes whose verified metadata.phone_number_id is the
//     channel's own: inbound messages become message (or reaction)
//     envelopes, outbound message statuses (sent, delivered, read, played,
//     failed) become status envelopes carrying delivery evidence in a
//     StatusSchema payload. Inbound media is looked up and downloaded with
//     the access token, which reaches only the Graph origin and
//     meta.MediaHost, and staged as an attachment when the sink stages
//     attachments. A user's reaction is a reaction.added envelope naming the
//     message reacted to and the emoji; a removal, which WhatsApp reports
//     without an emoji, is a reaction.removed without content, resolved by
//     the engine against the sender's one reaction on the message.
//   - Send posts one text or one template message to the stored route. A
//     text (free-form) message is only allowed inside the 24-hour customer
//     service window that a user's message opens; outside it a template
//     message (a structured part of TemplateSchema) is required. Meta
//     enforces the window and its refusal maps to rejected; when the host
//     gives the instance protocol state, the adapter records the window
//     from verified inbound messages and refuses a free-form message
//     after a recorded window closed before any provider call; for a user
//     with no recorded inbound message Meta decides.
//   - React sends a reaction message (type reaction) to the user, naming the
//     provider message reacted to (an inbound or an outbound one) and the
//     emoji; a removal sends an empty emoji. A user holds one reaction per
//     message (Exchange.Reactions is single), so a new emoji replaces the
//     old one and a removal needs none. Inbound, the same rule applies to
//     a user's reaction messages: a second emoji replaces the first, and
//     the removal, which names no emoji, removes the one reaction. A reaction message follows the
//     customer service window like a free-form message. Edit and delete
//     are not declared: the Cloud API has no call to change or take back a
//     message already sent.
//
// There is no reconciler: the Cloud API has no idempotency key and no way
// to look up or list sent messages, so a lost answer stays outcome_unknown
// and is never resent. Such a send settles only through status webhooks:
// each send carries its attempt and operation in biz_opaque_callback_data,
// Meta echoes it in status notifications, and the status envelope then
// names the attempt (model.EchoRef) next to the wamid and carries the
// delivery report (Envelope.Delivery) the engine settles it from. A
// reaction message carries its attempt the same way.
//
// Credentials come from ports.Credentials under the channel's credential
// reference, one purpose each: PurposeGraph (access token, OAuth2 or
// secret), PurposeAppSecret (signing secret) and PurposeVerifyToken
// (subscription verify token).
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Namespace is the identity and route namespace of WhatsApp users.
const Namespace = "meta.whatsapp"

// RouteKind is the ExternalRef kind of a WhatsApp user route.
const RouteKind = "user"

// Schemas.
const (
	// ConfigSchema is the channel configuration: Config as JSON.
	ConfigSchema  = "meta.whatsapp.config"
	ConfigVersion = 1
	// TemplateSchema is an outbound template message: the Cloud API
	// template object ({"name", "language": {"code"}, "components"}).
	TemplateSchema  = "meta.whatsapp.template"
	TemplateVersion = 1
	// MediaSchema is an inbound media message part: type, media id,
	// mime_type, sha256 and filename. When the sink stages attachments
	// (adapter.AttachmentSink), the bytes are downloaded and staged, and an
	// attachment part follows it.
	MediaSchema  = "meta.whatsapp.media"
	MediaVersion = 1
	// MessageSchema is an inbound message of a type without a text form:
	// its type and, for buttons and interactive replies, the payload IDs.
	MessageSchema  = "meta.whatsapp.message"
	MessageVersion = 1
	// InboundSchema is the payload of inbound message envelopes: the
	// sender's profile name and wa_id as Meta reports them (claims, never
	// identity), the message type and its reply context.
	InboundSchema  = "meta.whatsapp.inbound"
	InboundVersion = 1
	// StatusSchema is the payload of status envelopes: Status.
	StatusSchema  = "meta.whatsapp.status"
	StatusVersion = 1
	// WindowSchema is the protocol state recording a user's last inbound
	// message time.
	WindowSchema  = "meta.whatsapp.window"
	WindowVersion = 1
)

// Credential purposes.
const (
	PurposeGraph       = "meta.graph"
	PurposeAppSecret   = "meta.app_secret"
	PurposeVerifyToken = "meta.webhook.verify_token" // #nosec G101 -- a purpose name, not a credential
)

// DefaultAPIVersion is the Graph API version used when Config leaves it
// empty.
const DefaultAPIVersion = "v24.0"

// ServiceWindow is the customer service window a user's message opens.
const ServiceWindow = 24 * time.Hour

// Config is the channel configuration.
type Config struct {
	// PhoneNumberID is the business phone number's Graph ID. Required.
	PhoneNumberID string `json:"phone_number_id"`
	// APIVersion is the Graph API version, e.g. "v24.0"; empty means
	// DefaultAPIVersion.
	APIVersion string `json:"api_version,omitempty"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c)
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// Options are the factory's host-side settings, fixed for every channel.
// Graph calls go through the host's HTTP client, Dependencies.HTTP; an
// instance opened without one fails every send without a call.
type Options struct {
	// BaseURL is the Graph API origin; empty means meta.DefaultBaseURL.
	// Access tokens are only ever sent to it. It is a host setting, never
	// channel configuration, so a channel cannot redirect its token.
	BaseURL string
}

// Factory is the meta.whatsapp adapter factory. It keeps no state across
// channels.
type Factory struct{ opts Options }

// NewFactory returns the factory.
func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// Capabilities are the declared capabilities. A send reaches any WhatsApp
// user, one no inbound message named included, so a send can start a
// conversation; outside the user's service window it must carry a
// template part, which Meta enforces.
func Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReact, model.CapabilityStartConversation}
}

// Describe returns the descriptor.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                model.TypeMetaWhatsapp,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        Capabilities(),
		Events:              []string{model.EventMessageReceived, model.EventMessageUpdated, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{Namespace},
		RequiredPermissions: []string{"whatsapp_business_messaging"},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider, Reactions: adapter.ReactionsSingle},
	}
}

// Validate checks a channel configuration without I/O.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	_, issues := parseConfig(c)
	return issues
}

func parseConfig(c model.Extension) (Config, []model.ValidationIssue) {
	issue := func(field, code, msg string) []model.ValidationIssue {
		return []model.ValidationIssue{{Field: field, Code: code, Message: msg}}
	}
	if c.Name != ConfigSchema || c.Version != ConfigVersion {
		return Config{}, issue("configuration", string(model.SchemaMismatch), "want "+ConfigSchema+" version 1")
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(c.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, issue("configuration.data", string(model.Validation), "must be a JSON object with phone_number_id and api_version")
	}
	if !digits(cfg.PhoneNumberID) {
		return Config{}, issue("configuration.data.phone_number_id", string(model.Validation), "required: the phone number's numeric Graph ID")
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = DefaultAPIVersion
	}
	if !meta.ValidVersion(cfg.APIVersion) {
		return Config{}, issue("configuration.data.api_version", string(model.Validation), "must look like v24.0")
	}
	return cfg, nil
}

func digits(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// Open opens an instance bound to channel. It makes no network call.
func (f *Factory) Open(ctx context.Context, channel model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.Ref.Validate(); err != nil {
		return nil, err
	}
	if channel.Type != model.TypeMetaWhatsapp {
		return nil, model.NewError(model.Validation, "channel type is not "+string(model.TypeMetaWhatsapp))
	}
	cfg, issues := parseConfig(channel.Configuration)
	if len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid channel configuration")
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock.Now
	}
	return &Instance{
		channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, protocol: deps.Protocol, now: now,
		graph: meta.Client{HTTP: deps.HTTP, BaseURL: f.opts.BaseURL, Version: cfg.APIVersion, Now: now},
	}, nil
}

// Instance is one open channel: receive_http, send and react.
type Instance struct {
	channel  model.Channel
	cfg      Config
	creds    ports.Credentials
	protocol adapter.ProtocolState
	graph    meta.Client
	now      func() time.Time

	mu     sync.Mutex
	closed bool
}

// Capabilities reports receive_http, send and react available.
func (x *Instance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	if err := x.ready(ctx); err != nil {
		return nil, err
	}
	var out []model.CapabilityStatus
	for _, c := range Capabilities() {
		out = append(out, model.CapabilityStatus{Name: c, Available: true})
	}
	return out, nil
}

// Close closes the instance; it is idempotent.
func (x *Instance) Close(context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.closed = true
	return nil
}

func (x *Instance) isClosed() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.closed
}

func (x *Instance) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.isClosed() {
		return model.NewError(model.Unavailable, "whatsapp instance is closed")
	}
	return nil
}

// secret resolves the channel credential for purpose as bytes. Graph
// access tokens may be OAuth2 or opaque secrets; the signing secret and
// verify token are secrets.
func (x *Instance) secret(ctx context.Context, purpose string) ([]byte, error) {
	if x.creds == nil {
		return nil, &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	accept := []ports.CredentialKind{ports.KindSecret}
	if purpose == PurposeGraph {
		accept = []ports.CredentialKind{ports.KindOAuth2, ports.KindSecret}
	}
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: purpose, Accept: accept,
	})
	if err != nil {
		return nil, err
	}
	switch v := c.(type) {
	case ports.Secret:
		if len(v.Bytes) > 0 {
			return v.Bytes, nil
		}
	case ports.OAuth2:
		if purpose == PurposeGraph && v.AccessToken != "" {
			return []byte(v.AccessToken), nil
		}
	}
	return nil, &model.Error{Code: model.Unsupported, SafeMessage: "credential kind not usable for " + purpose, Retry: model.NoRetry}
}

var (
	_ adapter.Factory      = (*Factory)(nil)
	_ adapter.HTTPReceiver = (*Instance)(nil)
	_ adapter.Sender       = (*Instance)(nil)
	_ adapter.Reactor      = (*Instance)(nil)
)
