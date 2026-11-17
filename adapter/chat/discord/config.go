package discord

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration.
const (
	ConfigSchema         = string(model.TypeDiscordChat) + ".config"
	ConfigVersion uint32 = 1
)

// Defaults of the provider endpoints.
const (
	// DefaultAPIURL is the REST root of API version 10.
	DefaultAPIURL = "https://discord.com/api/v10/"
	// DefaultGatewayURL is the gateway the receiver connects to when Get
	// Gateway Bot answers none; it also sets which hosts a gateway URL may
	// name (see trustedGateway).
	DefaultGatewayURL = "wss://gateway.discord.gg"
)

// MaxHistoryChannels bounds Config.HistoryChannels.
const MaxHistoryChannels = 10

// ReplayWindow is the default and widest tolerance of a signed request's
// timestamp.
const ReplayWindow = 5 * time.Minute

// Config is the data of a channel configuration. One channel is one bot
// application: its bot token and public key are what the credential port
// resolves, and every guild the bot is in is served by the channel.
type Config struct {
	// ApplicationID is the application, a snowflake. Required: it is the
	// destination of every envelope (bind an inbox to it), the application
	// interaction follow-ups are sent as, and interactions of any other
	// application are dropped.
	ApplicationID string `json:"application_id"`
	// BotUserID is the bot user, a snowflake (the user Get Current User
	// answers for the bot token). Required: what it authored is never
	// reported back, and a mention of it is what "mentioned" means.
	BotUserID string `json:"bot_user_id"`
	// APIURL is the REST root; empty is DefaultAPIURL. https without user
	// info, query or fragment. The bot token is only ever sent to it.
	APIURL string `json:"api_url,omitempty"`
	// GatewayURL is the gateway to connect to when Get Gateway Bot names
	// none; empty is DefaultGatewayURL. wss without user info or fragment.
	GatewayURL string `json:"gateway_url,omitempty"`
	// MentionsOnly reports a guild message only when it mentions the bot;
	// direct messages are always reported. Unset is true: the receiver then
	// identifies without the privileged MESSAGE_CONTENT intent, which
	// Discord fills message bodies for in exactly those cases. False asks
	// for MESSAGE_CONTENT, which the application must be granted in the
	// Developer Portal, or the gateway closes with 4014.
	MentionsOnly *bool `json:"mentions_only,omitempty"`
	// HistoryChannels lists the channels (IDs, at most MaxHistoryChannels)
	// a channel-wide history read covers. Empty refuses channel-wide reads;
	// a read of one conversation needs none.
	HistoryChannels []string `json:"history_channels,omitempty"`
	// WebhookToleranceSeconds is the replay window of signed interaction
	// requests, 1 to 300; zero is 300.
	WebhookToleranceSeconds int `json:"webhook_tolerance_seconds,omitempty"`
	// Receive is how the channel takes what Discord sends. ReceiveGateway
	// offers receive (Instance.Run) and withholds receive_http,
	// ReceiveInteractions offers receive_http (Instance.Handler) and
	// withholds receive. Empty offers both: the gateway carries messages
	// and, while the application has no Interactions Endpoint URL,
	// interactions; with one, interactions go to the handler only.
	Receive ReceiveMode `json:"receive,omitempty"`
}

// ReceiveMode is how a channel takes what Discord sends (Config.Receive).
type ReceiveMode string

// The receive modes. The adapter never sets the Interactions Endpoint URL:
// that is a setting of the application, for its owner.
const (
	// ReceiveGateway takes messages, edits, deletions, reactions and
	// interactions over the gateway.
	ReceiveGateway ReceiveMode = "gateway"
	// ReceiveInteractions takes interactions at the Interactions Endpoint
	// URL; plain messages never arrive there.
	ReceiveInteractions ReceiveMode = "interactions"
)

// offers reports whether the configuration lets the channel use capability.
func (c Config) offers(capability model.Capability) bool {
	switch capability {
	case model.CapabilityReceive:
		return c.Receive != ReceiveInteractions
	case model.CapabilityReceiveHTTP:
		return c.Receive != ReceiveGateway
	}
	return true
}

// mentionsOnly is MentionsOnly with its default.
func (c Config) mentionsOnly() bool { return c.MentionsOnly == nil || *c.MentionsOnly }

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // a Config always encodes
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// apiURL returns the normalized REST root, ending in "/".
func (c Config) apiURL() string {
	switch {
	case c.APIURL == "":
		return DefaultAPIURL
	case strings.HasSuffix(c.APIURL, "/"):
		return c.APIURL
	}
	return c.APIURL + "/"
}

func (c Config) gatewayURL() string {
	if c.GatewayURL == "" {
		return DefaultGatewayURL
	}
	return c.GatewayURL
}

func (c Config) tolerance() time.Duration {
	if c.WebhookToleranceSeconds == 0 {
		return ReplayWindow
	}
	return time.Duration(c.WebhookToleranceSeconds) * time.Second
}

func parseConfig(x model.Extension) (Config, []model.ValidationIssue) {
	var c Config
	if x.Name != ConfigSchema || x.Version != ConfigVersion {
		return c, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch),
			Message: "want " + ConfigSchema + " version 1",
		}}
	}
	if len(x.Data) > 0 {
		dec := json.NewDecoder(bytes.NewReader(x.Data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil || dec.More() {
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a discord.chat configuration")}
		}
	}
	var issues []model.ValidationIssue
	bad := func(field, msg string) {
		issues = append(issues, issue("configuration.data."+field, msg))
	}
	if !isSnowflake(c.ApplicationID) {
		bad("application_id", "required: the application ID, a snowflake")
	}
	if !isSnowflake(c.BotUserID) {
		bad("bot_user_id", "required: the bot user ID, a snowflake")
	}
	if c.APIURL != "" && !cleanURL(c.APIURL, "https", false) {
		bad("api_url", "must be an https URL without user info, query or fragment")
	}
	if c.GatewayURL != "" && !cleanURL(c.GatewayURL, "wss", false) {
		bad("gateway_url", "must be a wss URL without user info, query or fragment")
	}
	if len(c.HistoryChannels) > MaxHistoryChannels || slices.ContainsFunc(c.HistoryChannels, func(s string) bool { return !isSnowflake(s) }) || hasDuplicate(c.HistoryChannels) {
		bad("history_channels", "must list at most 10 distinct channel IDs")
	}
	if c.WebhookToleranceSeconds < 0 || c.WebhookToleranceSeconds > int(ReplayWindow/time.Second) {
		bad("webhook_tolerance_seconds", "must be 1 to 300 seconds")
	}
	switch c.Receive {
	case "", ReceiveGateway, ReceiveInteractions:
	default:
		bad("receive", "must be \"gateway\" or \"interactions\"")
	}
	return c, issues
}

// cleanURL reports whether raw is an absolute URL of scheme with a host and
// no user info or fragment, and no query unless query is allowed.
func cleanURL(raw, scheme string, query bool) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == scheme && u.Host != "" && u.User == nil && u.Fragment == "" && (query || u.RawQuery == "")
}

func hasDuplicate(ids []string) bool {
	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			return true
		}
	}
	return false
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
