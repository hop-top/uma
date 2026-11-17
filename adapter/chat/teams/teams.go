package teams

import (
	"context"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/teams/botauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Type is the adapter type key.
const Type = model.TypeMicrosoftTeams

// Namespaces of the references the adapter reports. Namespace holds
// conversations (RefKindRoom: a channel, a group chat or a personal chat,
// by its base conversation ID) and channel threads (RefKindThread: the root
// message's activity ID under its channel's conversation ID); UserNamespace
// is the identity namespace: users and bots by their Teams account ID
// ("29:..." for a user, "28:..." for a bot), and the route of a personal
// chat with a user.
const (
	Namespace     = "microsoft.teams"
	UserNamespace = "microsoft.teams.user"
	RefKindUser   = "user"
)

// CredentialPurposeConnector is the purpose under which the adapter asks
// ports.Credentials for the Bot Connector access token: an OAuth2 bearer
// token the host mints with the client credentials of the bot's app
// registration and refreshes itself. It is only ever sent to the
// configured service URL.
const CredentialPurposeConnector = "microsoft.teams.connector" // #nosec G101 -- purpose name, not a credential

// DefaultOpenIDMetadataURL is the Bot Framework OpenID metadata document
// that names the keys signing the tokens the Bot Connector service sends
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication).
const DefaultOpenIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"

// Options configure a Factory. They are host settings, never channel
// configuration: a channel cannot choose whose keys it trusts.
type Options struct {
	// OpenIDMetadataURL is the metadata document naming the signing keys of
	// inbound tokens; empty is DefaultOpenIDMetadataURL. Tests point it at a
	// stand-in.
	OpenIDMetadataURL string
	// Tokens, when set, supplies the Bot Connector token source of a
	// channel, for hosts that mint tokens in process (see package
	// botauth). A nil answer keeps the channel on the host's
	// ports.Credentials under CredentialPurposeConnector.
	Tokens func(model.Channel) TokenSource
}

// TokenSource mints and caches one bot's Bot Connector access token;
// *botauth.Source implements it. fresh says the Connector refused the last
// token.
type TokenSource interface {
	Token(ctx context.Context, fresh bool) (botauth.Token, error)
}

// Factory is the microsoft.teams adapter.Factory. It is safe for concurrent
// use. Its instances share one cache of the Bot Framework signing keys.
type Factory struct {
	opts Options
	keys *keyCache
}

// NewFactory returns the adapter factory.
func NewFactory(opts Options) *Factory {
	if opts.OpenIDMetadataURL == "" {
		opts.OpenIDMetadataURL = DefaultOpenIDMetadataURL
	}
	return &Factory{opts: opts, keys: &keyCache{metadataURL: opts.OpenIDMetadataURL}}
}

// Capabilities are the declared capabilities.
func Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityStartConversation,
		model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilitySignal,
	}
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
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
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
	var src TokenSource
	if f.opts.Tokens != nil {
		src = f.opts.Tokens(channel.Clone())
	}
	return &Instance{channel: channel.Clone(), cfg: cfg, creds: deps.Credentials, src: src, http: deps.HTTP, now: now, keys: f.keys}, nil
}

// Instance is one open channel.
type Instance struct {
	channel model.Channel
	cfg     Config
	creds   ports.Credentials
	src     TokenSource
	http    ports.HTTPClient
	now     func() time.Time
	keys    *keyCache

	mu     sync.Mutex
	closed bool
}

// Capabilities reports every declared capability available: nothing in the
// configuration withholds one.
func (x *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range Capabilities() {
		out = append(out, model.CapabilityStatus{Name: c, Available: true})
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
)
