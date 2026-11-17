package google

import (
	"bytes"
	"encoding/json"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration schema.
const (
	ConfigSchema  = string(model.TypeGoogleMail) + ".config"
	ConfigVersion = 1
)

const (
	defaultEndpoint          = "https://gmail.googleapis.com/gmail/v1"
	defaultPollInterval      = 5 * time.Minute
	defaultInitialSyncWindow = time.Hour
	// Gmail stops notifying a watch not renewed within 7 days and
	// recommends renewing it once a day.
	defaultWatchRenewInterval = 24 * time.Hour
	maxWatchRenewInterval     = 6 * 24 * time.Hour
	minWatchRenewInterval     = time.Hour
)

// topicPattern is a Cloud Pub/Sub topic resource name.
var topicPattern = regexp.MustCompile(`^projects/[a-z][a-z0-9.:-]{4,61}[a-z0-9]/topics/[A-Za-z][A-Za-z0-9._~+%-]{2,254}$`)

// Config is the channel configuration (schema ConfigSchema, version
// ConfigVersion), encoded as JSON in model.Channel.Configuration.Data. It
// holds no secret: the access token is resolved through ports.Credentials
// and push requests are authenticated by Google-signed tokens.
type Config struct {
	// User is the Gmail userId of the mailbox: an email address, or empty
	// for "me", the user the access token belongs to.
	User string `json:"user"`
	// Address is the mailbox's own address, the From of outbound mail.
	// Empty reads it once from the Gmail profile.
	Address string `json:"address,omitempty"`
	// Addresses are the recipient addresses this mailbox answers to,
	// primary first. An inbound message's destination is the first of them
	// found in its To or Cc recipients, else the first one, else Address,
	// else User.
	Addresses []string `json:"addresses,omitempty"`
	// Subject is the subject of messages that start a thread; replies take
	// the parent's subject.
	Subject string `json:"subject,omitempty"`
	// MessageIDDomain is the right-hand side of outbound Message-IDs;
	// empty uses the domain of the From address.
	MessageIDDomain string `json:"message_id_domain,omitempty"`
	// Topic is the Cloud Pub/Sub topic users.watch publishes mailbox
	// changes to, "projects/<project>/topics/<topic>". Empty disables push:
	// the receiver then relies on history polling alone.
	Topic string `json:"topic,omitempty"`
	// PushAudience is the audience the push subscription's OIDC token
	// carries (by default the push endpoint URL). Required with Topic.
	PushAudience string `json:"push_audience,omitempty"`
	// PushServiceAccount is the email of the service account the push
	// subscription authenticates as. Required with Topic: any Google
	// service account can mint a token for any audience, so the audience
	// alone does not identify the subscription.
	PushServiceAccount string `json:"push_service_account,omitempty"`
	// PollIntervalMS is the wait between history rounds when no push
	// arrives. Zero is five minutes.
	PollIntervalMS int64 `json:"poll_interval_ms,omitempty"`
	// InitialSyncWindowMS bounds a full synchronization (no stored
	// historyId, or one Gmail no longer holds) to inbox messages received
	// that long before it starts. Zero is one hour.
	InitialSyncWindowMS int64 `json:"initial_sync_window_ms,omitempty"`
	// WatchRenewIntervalMS is how often the watch is renewed. Zero is one
	// day, as Gmail recommends; at most six days.
	WatchRenewIntervalMS int64 `json:"watch_renew_interval_ms,omitempty"`
	// HistoryMailboxes are the labels (IDs: system labels such as INBOX
	// or STARRED, user labels such as Label_12) a history import walks. A
	// message that carries any of them is read once. Empty walks the
	// receive scope, INBOX. SPAM, TRASH, DRAFT and SENT are refused.
	HistoryMailboxes []string `json:"history_mailboxes,omitempty"`
}

// refusedHistoryLabels are the labels a history import never walks, even
// when listed: junk, deleted mail, drafts and sent mail.
var refusedHistoryLabels = []string{labelSpam, labelTrash, labelDraft, labelSent}

// historyLabels are the labels a history read walks, sorted and without
// repeats: INBOX when none is configured.
func (c Config) historyLabels() []string {
	if len(c.HistoryMailboxes) == 0 {
		return []string{labelInbox}
	}
	out := slices.Clone(c.HistoryMailboxes)
	slices.Sort(out)
	return slices.Compact(out)
}

func (c Config) userID() string {
	if c.User == "" {
		return "me"
	}
	return c.User
}

func (c Config) pollInterval() time.Duration { return msOr(c.PollIntervalMS, defaultPollInterval) }

func (c Config) initialSyncWindow() time.Duration {
	return msOr(c.InitialSyncWindowMS, defaultInitialSyncWindow)
}

func (c Config) watchRenewInterval() time.Duration {
	return msOr(c.WatchRenewIntervalMS, defaultWatchRenewInterval)
}

// push reports whether Gmail push notifications are configured.
func (c Config) push() bool { return c.Topic != "" }

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
	if c.User != "" && c.User != "me" && !bareAddress(c.User) {
		out = append(out, issue("user", "must be me or the mailbox address"))
	}
	if c.Address != "" && !bareAddress(c.Address) {
		out = append(out, issue("address", "must be a bare email address"))
	}
	for _, a := range c.Addresses {
		if !plainToken(a) {
			out = append(out, issue("addresses", "must be recipient addresses"))
			break
		}
	}
	if strings.ContainsAny(c.Subject, "\r\n") || len(c.Subject) > 998 {
		out = append(out, issue("subject", "must be one line"))
	}
	if c.MessageIDDomain != "" && !hostname(c.MessageIDDomain) {
		out = append(out, issue("message_id_domain", "must be a host name"))
	}
	switch {
	case c.Topic == "" && (c.PushAudience != "" || c.PushServiceAccount != ""):
		out = append(out, issue("topic", "required with push_audience and push_service_account"))
	case c.Topic != "":
		if !topicPattern.MatchString(c.Topic) {
			out = append(out, issue("topic", "must be projects/<project>/topics/<topic>"))
		}
		if c.PushAudience == "" || len(c.PushAudience) > 2048 || strings.ContainsFunc(c.PushAudience, isSpaceOrControl) {
			out = append(out, issue("push_audience", "required with topic"))
		}
		if !bareAddress(c.PushServiceAccount) {
			out = append(out, issue("push_service_account", "must be the push service account email"))
		}
	}
	if c.PollIntervalMS < 0 {
		out = append(out, issue("poll_interval_ms", "must not be negative"))
	}
	if c.InitialSyncWindowMS < 0 {
		out = append(out, issue("initial_sync_window_ms", "must not be negative"))
	}
	if c.WatchRenewIntervalMS < 0 || (c.WatchRenewIntervalMS > 0 &&
		(c.watchRenewInterval() < minWatchRenewInterval || c.watchRenewInterval() > maxWatchRenewInterval)) {
		out = append(out, issue("watch_renew_interval_ms", "must be between one hour and six days"))
	}
	for _, l := range c.HistoryMailboxes {
		if !plainToken(l) || slices.ContainsFunc(refusedHistoryLabels, func(r string) bool { return strings.EqualFold(r, l) }) {
			out = append(out, issue("history_mailboxes", "must be label IDs other than SPAM, TRASH, DRAFT and SENT"))
			break
		}
	}
	return c, out
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// plainToken reports a non-empty value without whitespace, control
// characters, quotes or path separators.
func plainToken(s string) bool {
	if s == "" || len(s) > 320 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return isSpaceOrControl(r) || r == '/' || r == '\\' || r == '\'' || r == '"' || r == '?' || r == '#'
	})
}

// bareAddress reports whether s is an addr-spec without display name,
// brackets or surrounding space.
func bareAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s
}

// hostname reports whether s is a DNS host name.
func hostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
