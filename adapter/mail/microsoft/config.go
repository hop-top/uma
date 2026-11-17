package microsoft

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration schema.
const (
	ConfigSchema  = string(model.TypeMicrosoftMail) + ".config"
	ConfigVersion = 1
)

// Clouds: the Microsoft Graph national cloud deployments a channel can
// address. CloudGlobal is the default.
const (
	CloudGlobal     = "global"
	CloudUSGov      = "usgov"
	CloudUSGovDoD   = "usgov_dod"
	CloudChina      = "china"
	defaultEndpoint = "https://graph.microsoft.com/v1.0"
)

var cloudEndpoints = map[string]string{
	CloudGlobal:   defaultEndpoint,
	CloudUSGov:    "https://graph.microsoft.us/v1.0",
	CloudUSGovDoD: "https://dod-graph.microsoft.us/v1.0",
	CloudChina:    "https://microsoftgraph.chinacloudapi.cn/v1.0",
}

// Subscription lifetime bounds for Outlook messages: Graph raises anything
// under 45 minutes to 45 minutes and caps basic notifications at 10,080
// minutes (under seven days).
const (
	minSubscriptionLifetime     = 45 * time.Minute
	maxSubscriptionLifetime     = 10080*time.Minute - time.Minute
	defaultSubscriptionLifetime = 72 * time.Hour
	defaultPollInterval         = 5 * time.Minute
	defaultInitialSyncWindow    = time.Hour
)

// Config is the channel configuration (schema ConfigSchema, version
// ConfigVersion), encoded as JSON in model.Channel.Configuration.Data. It
// holds no secret: the access token and the notification client state are
// resolved through ports.Credentials.
type Config struct {
	// Mailbox is the user ID or user principal name of the mailbox
	// (/users/{mailbox}). Empty addresses the signed-in user (/me), which
	// needs a delegated token.
	Mailbox string `json:"mailbox"`
	// Addresses are the recipient addresses this mailbox answers to, primary
	// first. An inbound message's destination is the first of them found in
	// its To or Cc recipients, else the first one, else Mailbox.
	Addresses []string `json:"addresses,omitempty"`
	// Cloud selects the national cloud endpoint; empty is CloudGlobal.
	Cloud string `json:"cloud"`
	// NotificationURL is the public HTTPS URL where the host mounts the
	// channel's webhook (Runtime.Webhook). Empty disables change
	// notifications: the receiver then relies on delta polling alone.
	NotificationURL string `json:"notification_url"`
	// LifecycleNotificationURL receives lifecycle notifications; empty uses
	// NotificationURL (the webhook serves both).
	LifecycleNotificationURL string `json:"lifecycle_notification_url"`
	// SubscriptionLifetimeMS is the lifetime requested for each
	// subscription create or renewal. Zero is three days.
	SubscriptionLifetimeMS int64 `json:"subscription_lifetime_ms"`
	// PollIntervalMS is the wait between delta rounds when no notification
	// arrives. Zero is five minutes.
	PollIntervalMS int64 `json:"poll_interval_ms"`
	// InitialSyncWindowMS bounds the first delta round of a receiver with no
	// stored delta state to messages received that long before it starts.
	// Zero is one hour.
	InitialSyncWindowMS int64 `json:"initial_sync_window_ms"`
	// WatchedFolders are folders watched for provider state in addition to
	// the inbox, Archive and Deleted Items, each a mail folder ID or a
	// well-known folder name (for example "junkemail"). A folder the
	// mailbox does not have is skipped.
	WatchedFolders []string `json:"watched_folders,omitempty"`
	// UndoFolder is where unarchive and untrash move messages: a mail folder
	// ID or a well-known folder name. Empty is the inbox: Graph keeps no
	// record of the folder a message came from.
	UndoFolder string `json:"undo_folder,omitempty"`
	// HistoryMailboxes are the folders a history import walks, in this
	// order, each a mail folder ID or a well-known folder name (for example
	// "inbox", "archive"). Empty walks the receive scope, the inbox. Junk
	// Email, Deleted Items, Drafts, Sent Items and the Outbox are refused,
	// by name here and by ID when a folder is read.
	HistoryMailboxes []string `json:"history_mailboxes,omitempty"`
}

// refusedHistoryFolders are the well-known folders a history import never
// walks, even when listed: junk, deleted mail, drafts, sent mail and the
// outbox.
var refusedHistoryFolders = []string{folderJunk, folderTrash, "drafts", "sentitems", "outbox"}

// historyFolders are the folders a history read walks, in order: the inbox
// when none is configured.
func (c Config) historyFolders() []string {
	if len(c.HistoryMailboxes) == 0 {
		return []string{folderInbox}
	}
	return c.HistoryMailboxes
}

func (c Config) endpoint() string {
	if e, ok := cloudEndpoints[c.Cloud]; ok {
		return e
	}
	return defaultEndpoint
}

func (c Config) subscriptionLifetime() time.Duration {
	return msOr(c.SubscriptionLifetimeMS, defaultSubscriptionLifetime)
}

func (c Config) pollInterval() time.Duration { return msOr(c.PollIntervalMS, defaultPollInterval) }

func (c Config) initialSyncWindow() time.Duration {
	return msOr(c.InitialSyncWindowMS, defaultInitialSyncWindow)
}

func (c Config) lifecycleURL() string {
	if c.LifecycleNotificationURL != "" {
		return c.LifecycleNotificationURL
	}
	return c.NotificationURL
}

// selfAddresses are the addresses that are this mailbox's own: Addresses
// and a Mailbox that is an address. A reply to all never sends to them.
func (c Config) selfAddresses() []string {
	out := slices.Clone(c.Addresses)
	if strings.Contains(c.Mailbox, "@") {
		out = append(out, c.Mailbox)
	}
	return out
}

// mailboxPath is the path segment addressing the mailbox.
func (c Config) mailboxPath() string {
	if c.Mailbox == "" {
		return "/me"
	}
	return "/users/" + url.PathEscape(c.Mailbox)
}

// subscriptionResource is the inbox message resource subscriptions watch.
func (c Config) subscriptionResource() string {
	if c.Mailbox == "" {
		return "me/mailFolders('inbox')/messages"
	}
	return "users/" + c.Mailbox + "/mailFolders('inbox')/messages"
}

func msOr(ms int64, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

// parseConfig decodes and checks a channel configuration.
func parseConfig(e model.Extension) (Config, []model.ValidationIssue) {
	var c Config
	if e.Name != ConfigSchema || e.Version != ConfigVersion {
		return c, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch),
			Message: "want " + ConfigSchema + " version 1",
		}}
	}
	if len(e.Data) > 0 {
		dec := json.NewDecoder(bytes.NewReader(e.Data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, []model.ValidationIssue{issue("configuration.data", "must be a "+ConfigSchema+" JSON object")}
		}
	}
	var out []model.ValidationIssue
	if c.Mailbox != "" && !plainToken(c.Mailbox) {
		out = append(out, issue("mailbox", "must be a user ID or user principal name"))
	}
	for _, a := range c.Addresses {
		if !plainToken(a) {
			out = append(out, issue("addresses", "must be recipient addresses"))
			break
		}
	}
	if _, ok := cloudEndpoints[c.Cloud]; c.Cloud != "" && !ok {
		out = append(out, issue("cloud", "must be global, usgov, usgov_dod or china"))
	}
	if c.NotificationURL != "" && !httpsURL(c.NotificationURL) {
		out = append(out, issue("notification_url", "must be an absolute https URL"))
	}
	if c.LifecycleNotificationURL != "" && (c.NotificationURL == "" || !httpsURL(c.LifecycleNotificationURL)) {
		out = append(out, issue("lifecycle_notification_url", "must be an absolute https URL next to notification_url"))
	}
	if c.SubscriptionLifetimeMS < 0 || (c.SubscriptionLifetimeMS > 0 &&
		(c.subscriptionLifetime() < minSubscriptionLifetime || c.subscriptionLifetime() > maxSubscriptionLifetime)) {
		out = append(out, issue("subscription_lifetime_ms", "must be between 45 minutes and just under 7 days"))
	}
	for _, f := range c.WatchedFolders {
		if !plainToken(f) {
			out = append(out, issue("watched_folders", "must be mail folder IDs or well-known folder names"))
			break
		}
	}
	if c.UndoFolder != "" && !plainToken(c.UndoFolder) {
		out = append(out, issue("undo_folder", "must be a mail folder ID or a well-known folder name"))
	}
	for n, f := range c.HistoryMailboxes {
		if !plainToken(f) || slices.ContainsFunc(refusedHistoryFolders, func(r string) bool { return strings.EqualFold(r, f) }) ||
			slices.Contains(c.HistoryMailboxes[:n], f) {
			out = append(out, issue("history_mailboxes", "must be distinct mail folder IDs or well-known folder names other than junkemail, deleteditems, drafts, sentitems and outbox"))
			break
		}
	}
	if c.PollIntervalMS < 0 {
		out = append(out, issue("poll_interval_ms", "must not be negative"))
	}
	if c.InitialSyncWindowMS < 0 {
		out = append(out, issue("initial_sync_window_ms", "must not be negative"))
	}
	return c, out
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}

// plainToken reports a non-empty value without whitespace, control
// characters, quotes or path separators.
func plainToken(s string) bool {
	if s == "" || len(s) > 320 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == '/' || r == '\\' || r == '\'' || r == '"' || r == '?' || r == '#'
	})
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
