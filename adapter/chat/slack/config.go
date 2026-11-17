package slack

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration.
const (
	ConfigSchema         = string(model.TypeSlackChat) + ".config"
	ConfigVersion uint32 = 1
)

// DefaultAPIURL is the Web API root of slack.com.
const DefaultAPIURL = "https://slack.com/api/"

// MaxHistoryChannels bounds Config.HistoryChannels: a channel-wide history
// page reads every listed conversation.
const MaxHistoryChannels = 10

// Config is the data of a channel configuration. One channel is one
// workspace install of one app: the signing secret and the bot token the
// credential port resolves belong to that app, and Slack events of any
// other workspace are not this channel's.
type Config struct {
	// TeamID is the workspace (or Enterprise Grid org) ID, such as T0123.
	// Required. Events, interactions and slash commands naming another
	// team are acknowledged and dropped. It is also the destination of
	// every envelope: bind an inbox to it.
	TeamID string `json:"team_id"`
	// BotUserID is the app's bot user, such as U0123, from auth.test.
	// Required: messages and reactions it authored are never reported
	// back, which would loop, and a mention of it is what "mentioned"
	// means.
	BotUserID string `json:"bot_user_id"`
	// BotID is the app's bot, such as B0123, for the legacy bot_message
	// events that name a bot and no user. Optional.
	BotID string `json:"bot_id,omitempty"`
	// AppID restricts events and interactions to this app (api_app_id),
	// such as A0123. Optional.
	AppID string `json:"app_id,omitempty"`
	// APIURL is the Web API root; empty is DefaultAPIURL. It must be https
	// without user info, query or fragment. The bot token is only ever
	// sent to it.
	APIURL string `json:"api_url,omitempty"`
	// MentionsOnly reports a message of a public channel, private channel
	// or group direct message only when it mentions the bot (the
	// app_mention event); direct messages are always reported.
	MentionsOnly bool `json:"mentions_only,omitempty"`
	// HistoryChannels lists the conversations (IDs, at most
	// MaxHistoryChannels) a channel-wide history read covers: Slack has no
	// workspace-wide history. Empty refuses channel-wide reads; a read of
	// one conversation needs none.
	HistoryChannels []string `json:"history_channels,omitempty"`
	// EvaluateInteractions asks the host's interaction validator, inside
	// Slack's three seconds, about a modal submission before it is recorded
	// (its field errors answer the modal) and about an external select's
	// option request (its options answer the menu). Off, a submission is
	// recorded and acknowledged with an empty 200 and an option request gets
	// an empty list. On, a host with no validator denies both.
	EvaluateInteractions bool `json:"evaluate_interactions,omitempty"`
	// WebhookToleranceSeconds is the replay window of signed requests, 1
	// to 300; zero is 300, the five minutes Slack's documentation checks.
	WebhookToleranceSeconds int `json:"webhook_tolerance_seconds,omitempty"`
	// Receive is how the app takes events and interactions. Slack sends an
	// app's payloads to its request URL or, with Socket Mode on, over its
	// WebSocket connections, never both ("When you toggle Socket Mode on,
	// you'll only receive events and interactive payloads over your
	// WebSocket connections—not over HTTP",
	// https://docs.slack.dev/apis/events-api/using-socket-mode/), and the
	// switch is in the app's settings, so the configuration says which:
	// ReceiveEventsAPI offers receive_http (Instance.Handler) and withholds
	// receive, ReceiveSocketMode offers receive (Instance.Run) and withholds
	// receive_http. Empty offers both and leaves the choice to the host.
	Receive ReceiveMode `json:"receive,omitempty"`
}

// ReceiveMode is how a channel takes the app's payloads (Config.Receive).
type ReceiveMode string

// The receive modes. The adapter never turns Socket Mode on or off: that
// is a setting of the app, for its owner.
const (
	// ReceiveEventsAPI takes payloads at the app's request URL.
	ReceiveEventsAPI ReceiveMode = "events_api"
	// ReceiveSocketMode takes payloads over a Socket Mode connection the
	// receiver opens with the app-level token.
	ReceiveSocketMode ReceiveMode = "socket_mode"
)

// offers reports whether the configuration lets the channel use
// capability: a receive mode withholds the other way of receiving, and
// nothing else.
func (c Config) offers(capability model.Capability) bool {
	switch capability {
	case model.CapabilityReceive:
		return c.Receive != ReceiveEventsAPI
	case model.CapabilityReceiveHTTP:
		return c.Receive != ReceiveSocketMode
	}
	return true
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // a Config always encodes
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// apiURL returns the normalized Web API root, ending in "/".
func (c Config) apiURL() string {
	switch {
	case c.APIURL == "":
		return DefaultAPIURL
	case strings.HasSuffix(c.APIURL, "/"):
		return c.APIURL
	}
	return c.APIURL + "/"
}

// tolerance is the replay window.
func (c Config) tolerance() time.Duration {
	if c.WebhookToleranceSeconds == 0 {
		return ReplayWindow
	}
	return time.Duration(c.WebhookToleranceSeconds) * time.Second
}

// slackID is the shape of Slack IDs (T0123, U0123, C0123, B0123, A0123):
// an uppercase letter and uppercase letters or digits.
var slackID = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,31}$`)

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
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a slack.chat configuration")}
		}
	}
	var issues []model.ValidationIssue
	bad := func(field, msg string) {
		issues = append(issues, issue("configuration.data."+field, msg))
	}
	if !slackID.MatchString(c.TeamID) {
		bad("team_id", "required: the workspace ID, such as T0123")
	}
	if !slackID.MatchString(c.BotUserID) {
		bad("bot_user_id", "required: the app's bot user ID, such as U0123")
	}
	if c.BotID != "" && !slackID.MatchString(c.BotID) {
		bad("bot_id", "must be a Slack bot ID, such as B0123")
	}
	if c.AppID != "" && !slackID.MatchString(c.AppID) {
		bad("app_id", "must be a Slack app ID, such as A0123")
	}
	if c.APIURL != "" {
		u, err := url.Parse(c.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			bad("api_url", "must be an https URL without user info, query or fragment")
		}
	}
	if !validHistoryChannels(c.HistoryChannels) {
		bad("history_channels", "must list at most 10 distinct conversation IDs")
	}
	if c.WebhookToleranceSeconds < 0 || c.WebhookToleranceSeconds > int(ReplayWindow/time.Second) {
		bad("webhook_tolerance_seconds", "must be 1 to 300 seconds")
	}
	switch c.Receive {
	case "", ReceiveEventsAPI, ReceiveSocketMode:
	default:
		bad("receive", "must be \"events_api\" or \"socket_mode\"")
	}
	return c, issues
}

func validHistoryChannels(ids []string) bool {
	if len(ids) > MaxHistoryChannels {
		return false
	}
	for i, id := range ids {
		if !slackID.MatchString(id) || slices.Contains(ids[:i], id) {
			return false
		}
	}
	return true
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
