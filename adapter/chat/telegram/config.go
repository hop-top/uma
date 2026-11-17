package telegram

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration.
const (
	ConfigSchema         = string(model.TypeTelegramMessages) + ".config"
	ConfigVersion uint32 = 1
)

// DefaultAPIURL is the Bot API root of telegram.org. Requests go to
// <root>bot<token>/<method>.
const DefaultAPIURL = "https://api.telegram.org/"

// Poll timeouts, in seconds, of the long-polling receiver (getUpdates
// takes any value; the adapter bounds it so one request cannot outlive a
// host's HTTP client for good).
const (
	DefaultPollTimeoutSeconds = 30
	MaxPollTimeoutSeconds     = 50
)

// Config is the data of a channel configuration. One channel is one bot:
// the bot token and the webhook secret the credential port resolves belong
// to it, and updates of any other bot are not this channel's.
type Config struct {
	// BotID is the bot's own user ID, the number before the colon of its
	// token and the ID getMe answers. Required: messages it authored are
	// never reported back, and a reply to one of its messages is what
	// "replied to the bot" means. It is also the destination of every
	// envelope: bind an inbox to it, as a decimal string.
	BotID int64 `json:"bot_id"`
	// BotUsername is the bot's username without "@", such as "uma_bot".
	// Required: it is what a mention and a command addressed to the bot
	// ("/start@uma_bot") name.
	BotUsername string `json:"bot_username"`
	// APIURL is the Bot API root; empty is DefaultAPIURL. It must be https
	// without user info, query or fragment. The bot token, which Telegram
	// takes in the request path, is only ever sent to it.
	APIURL string `json:"api_url,omitempty"`
	// MentionsOnly reports a message of a group, supergroup or channel only
	// when it addresses the bot: it mentions it, replies to one of its
	// messages or is a command for it. Private chats are always reported.
	MentionsOnly bool `json:"mentions_only,omitempty"`
	// PollTimeoutSeconds is the long-polling timeout of the receiver, 1 to
	// MaxPollTimeoutSeconds; zero is DefaultPollTimeoutSeconds.
	PollTimeoutSeconds int `json:"poll_timeout_seconds,omitempty"`
	// Receive is how the bot takes updates. Telegram serves a bot by
	// getUpdates or by its webhook, never both, and which one is the bot's
	// is set at Telegram, so the configuration says it: ReceivePoll offers
	// receive (Instance.Run) and withholds receive_http, ReceiveWebhook
	// offers receive_http (Instance.Handler) and withholds receive. Empty
	// offers both and leaves the choice to the host.
	Receive ReceiveMode `json:"receive,omitempty"`
}

// ReceiveMode is how a channel takes the bot's updates (Config.Receive).
type ReceiveMode string

// The receive modes. The adapter never sets or deletes a webhook in
// either: that is the bot owner's decision, made with setWebhook and
// deleteWebhook outside uma.
const (
	// ReceivePoll reads updates by long polling (getUpdates). Telegram
	// refuses every read while a webhook is set.
	ReceivePoll ReceiveMode = "poll"
	// ReceiveWebhook takes updates at the bot's webhook.
	ReceiveWebhook ReceiveMode = "webhook"
)

// offers reports whether the configuration lets the channel use
// capability: a receive mode withholds the other way of receiving, and
// nothing else.
func (c Config) offers(capability model.Capability) bool {
	switch capability {
	case model.CapabilityReceive:
		return c.Receive != ReceiveWebhook
	case model.CapabilityReceiveHTTP:
		return c.Receive != ReceivePoll
	}
	return true
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // a Config always encodes
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// apiURL returns the normalized Bot API root, ending in "/".
func (c Config) apiURL() string {
	switch {
	case c.APIURL == "":
		return DefaultAPIURL
	case strings.HasSuffix(c.APIURL, "/"):
		return c.APIURL
	}
	return c.APIURL + "/"
}

func (c Config) pollTimeout() time.Duration {
	if c.PollTimeoutSeconds == 0 {
		return DefaultPollTimeoutSeconds * time.Second
	}
	return time.Duration(c.PollTimeoutSeconds) * time.Second
}

// usernamePattern is a bot username: "5-32 characters long and not case
// sensitive – but may only include Latin characters, numbers, and
// underscores. Your bot's username must end in 'bot'"
// (https://core.telegram.org/bots/features#botfather).
var usernamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,28}[Bb][Oo][Tt]$`)

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
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a telegram.messages configuration")}
		}
	}
	var issues []model.ValidationIssue
	bad := func(field, msg string) {
		issues = append(issues, issue("configuration.data."+field, msg))
	}
	if c.BotID <= 0 {
		bad("bot_id", "required: the bot's user ID, the number before the colon of its token")
	}
	if !usernamePattern.MatchString(c.BotUsername) {
		bad("bot_username", "required: the bot's username without \"@\", 5 to 32 letters, digits or \"_\", ending in \"bot\"")
	}
	if c.APIURL != "" {
		u, err := url.Parse(c.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			bad("api_url", "must be an https URL without user info, query or fragment")
		}
	}
	if c.PollTimeoutSeconds < 0 || c.PollTimeoutSeconds > MaxPollTimeoutSeconds {
		bad("poll_timeout_seconds", "must be 1 to 50 seconds")
	}
	switch c.Receive {
	case "", ReceivePoll, ReceiveWebhook:
	default:
		bad("receive", "must be \"poll\" or \"webhook\"")
	}
	return c, issues
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
