package imap

import (
	"bytes"
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
)

// Names this adapter owns.
const (
	// Type is the adapter type key.
	Type = model.TypeImapMail
	// ConfigSchema and ConfigVersion name the channel configuration.
	ConfigSchema  = "imap.mail.config"
	ConfigVersion = 1
	// Namespace is the identity and route namespace: a normalized mailbox
	// address (model.NormalizeEmail).
	Namespace = "imap.mail.address"
	// RouteKind is the kind of sender and route references.
	RouteKind = "address"
	// MessageSchema and MessageVersion name the envelope payload recorded as
	// message metadata: mailbox position and header fields.
	MessageSchema  = "imap.mail.message"
	MessageVersion = 1
	// PartsSchema and PartsVersion name the structured content part that
	// describes non-text MIME parts.
	PartsSchema  = "imap.mail.parts"
	PartsVersion = 1
	// PurposeIMAP and PurposeSubmission are the credential purposes of
	// mailbox retrieval and of SMTP submission. Both resolve the channel's
	// one credential reference.
	PurposeIMAP       = "imap"
	PurposeSubmission = "smtp.submission"
	// ObservationGap is the observation kind reported when the mailbox's
	// UIDVALIDITY changed: every stored UID is void and the receiver
	// resynchronizes (see Instance.Run).
	ObservationGap = "imap.mail.uidvalidity_gap"
	// ObservationVanished is the observation kind reported for a message the
	// mailbox no longer holds when the receiver comes to fetch it: expunged
	// after the search or after its metadata fetch. The receiver confirms with
	// a search by UID, skips the message and moves on.
	ObservationVanished = "imap.mail.message_vanished"
	// ObservationSkipped is the observation kind reported for a message the
	// receiver fetched and cannot deliver: not parseable as a message, or
	// without a usable From address. The receiver skips it and moves on.
	ObservationSkipped = "imap.mail.message_skipped"
)

// Configuration defaults and bounds.
const (
	DefaultMailbox           = "INBOX"
	DefaultIdleRestart       = 25 * time.Minute
	MaxIdleRestart           = 29 * time.Minute // RFC 2177
	DefaultPollInterval      = time.Minute
	DefaultInitialSyncWindow = time.Hour
	DefaultMaxMessageBytes   = 25 << 20
	DefaultCommandTimeout    = time.Minute
)

// Config is the channel configuration, the JSON data of a ConfigSchema
// extension.
type Config struct {
	// Address is the mailbox address: the From of outbound mail.
	Address string `json:"address"`
	// FromName is the optional display name, Subject the optional subject of
	// outbound mail.
	FromName string `json:"from_name,omitempty"`
	Subject  string `json:"subject,omitempty"`
	// Destination is the inbox destination inbound mail is delivered to;
	// empty takes Address.
	Destination string `json:"destination,omitempty"`
	// MessageIDDomain is the right-hand side of outbound Message-IDs; empty
	// takes Address's domain.
	MessageIDDomain string `json:"message_id_domain,omitempty"`
	// Mailbox is the mailbox retrieved; empty is INBOX.
	Mailbox string `json:"mailbox,omitempty"`
	// WatchMailboxes are further mailboxes whose messages the receiver keeps
	// state of, beside Mailbox: flags are followed and a message that leaves
	// one is looked up in the others. Nothing in them is delivered as new
	// mail. Mailboxes with the special use of archive, trash and junk are
	// searched for a message that left Mailbox whether or not they are
	// listed here, but only a listed one is followed afterwards.
	WatchMailboxes []string `json:"watch_mailboxes,omitempty"`
	// HistoryMailboxes are the mailboxes a history import walks, in this
	// order, each to its end: names as the server lists them. Empty walks
	// Mailbox, the receive scope. Mailboxes named like junk, trash, drafts
	// or sent mail, and Sent.Mailbox, are refused here; one the server marks
	// \Junk, \Trash, \Drafts or \Sent is refused when it is read.
	HistoryMailboxes []string `json:"history_mailboxes,omitempty"`
	// StateSyncIntervalMS is, on a server without CONDSTORE, how long the
	// receiver waits between two comparisons of the flags it holds with the
	// server's; zero takes PollIntervalMS's default of one minute.
	StateSyncIntervalMS int64 `json:"state_sync_interval_ms,omitempty"`
	// IMAP is the retrieval server, Submission the SMTP submission server.
	IMAP       Server `json:"imap"`
	Submission Server `json:"submission"`
	// Sent declares where sent copies live.
	Sent Sent `json:"sent,omitzero"`
	// ReconnectMarginMS is how long before credential expiry a session is
	// replaced; zero takes the package auth default (5 minutes).
	ReconnectMarginMS int64 `json:"reconnect_margin_ms,omitempty"`
	// IdleRestartMS is how long one IDLE runs before the receiver ends it
	// and checks the mailbox; zero is 25 minutes, at most 29 (RFC 2177).
	IdleRestartMS int64 `json:"idle_restart_ms,omitempty"`
	// PollIntervalMS is the wait between checks on a server without IDLE;
	// zero is one minute.
	PollIntervalMS int64 `json:"poll_interval_ms,omitempty"`
	// InitialSyncWindowMS bounds the first synchronization of a mailbox:
	// messages received earlier are not delivered. Zero is one hour.
	InitialSyncWindowMS int64 `json:"initial_sync_window_ms,omitempty"`
	// MaxMessageBytes bounds the message bytes fetched; a larger message
	// is delivered from its header alone. Zero is 25 MiB.
	MaxMessageBytes int64 `json:"max_message_bytes,omitempty"`
}

// Server is one IMAP or SMTP submission server. At least one port is set;
// implicit TLS is tried first, STARTTLS when it is unset or refuses the
// connection. PlainPort needs AllowPlaintextLoopback and is used only when
// no TLS port is set, and only to a loopback address.
type Server struct {
	Host                   string `json:"host"`
	TLSPort                int    `json:"tls_port,omitempty"`
	StartTLSPort           int    `json:"starttls_port,omitempty"`
	PlainPort              int    `json:"plain_port,omitempty"`
	AllowPlaintextLoopback bool   `json:"allow_plaintext_loopback,omitempty"`
	// CommandTimeoutMS bounds each command reply; DataTimeoutMS the
	// submission reply to the end of DATA. Zero takes the default.
	CommandTimeoutMS int64 `json:"command_timeout_ms,omitempty"`
	DataTimeoutMS    int64 `json:"data_timeout_ms,omitempty"`
}

// Sent declares the sent-copies mailbox. With Mailbox empty the adapter
// keeps no sent copy and cannot reconcile a lost submission reply. With
// SavedByProvider the server files a copy of every submitted message
// itself (Gmail, Microsoft 365); otherwise the adapter appends one after
// the submission server accepted the message.
type Sent struct {
	Mailbox         string `json:"mailbox,omitempty"`
	SavedByProvider bool   `json:"saved_by_provider,omitempty"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // plain struct: cannot fail
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// ParseConfig decodes and validates a channel configuration.
func ParseConfig(x model.Extension) (Config, []model.ValidationIssue) {
	if x.Name != ConfigSchema || x.Version != ConfigVersion {
		return Config{}, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch),
			Message: "want " + ConfigSchema + " version " + strconv.Itoa(ConfigVersion),
		}}
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(x.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a valid "+ConfigSchema+" object")}
	}
	return c, c.validate()
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}

func (c Config) validate() []model.ValidationIssue {
	var out []model.ValidationIssue
	if norm, err := model.NormalizeEmail(c.Address); err != nil || norm != c.Address {
		out = append(out, issue("address", "must be a normalized bare address"))
	}
	for field, v := range map[string]string{"from_name": c.FromName, "subject": c.Subject} {
		if strings.ContainsAny(v, "\r\n") || !utf8.ValidString(v) {
			out = append(out, issue(field, "must be one line of UTF-8"))
		}
	}
	if c.Destination != strings.TrimSpace(c.Destination) {
		out = append(out, issue("destination", "must not have surrounding space"))
	}
	if c.MessageIDDomain != "" && !hostname(c.MessageIDDomain) {
		out = append(out, issue("message_id_domain", "must be a host name"))
	}
	if !mailboxName(c.Mailbox, true) {
		out = append(out, issue("mailbox", "must be a mailbox name"))
	}
	out = append(out, c.IMAP.validate("imap")...)
	out = append(out, c.Submission.validate("submission")...)
	switch {
	case !mailboxName(c.Sent.Mailbox, true):
		out = append(out, issue("sent.mailbox", "must be a mailbox name"))
	case c.Sent.SavedByProvider && c.Sent.Mailbox == "":
		out = append(out, issue("sent.saved_by_provider", "needs sent.mailbox"))
	}
	for k, w := range c.WatchMailboxes {
		if !mailboxName(w, false) || w == c.mailbox() || slices.Contains(c.WatchMailboxes[:k], w) {
			out = append(out, issue("watch_mailboxes", "must be distinct mailbox names other than mailbox"))
			break
		}
	}
	for k, h := range c.HistoryMailboxes {
		if !mailboxName(h, false) || strings.ContainsAny(h, "*%") || refusedHistoryMailbox(h) || (c.Sent.Mailbox != "" && strings.EqualFold(h, c.Sent.Mailbox)) ||
			slices.Contains(c.HistoryMailboxes[:k], h) {
			out = append(out, issue("history_mailboxes", "must be distinct mailbox names, not sent, drafts, junk or trash"))
			break
		}
	}
	for field, v := range map[string]int64{
		"state_sync_interval_ms": c.StateSyncIntervalMS,
		"reconnect_margin_ms":    c.ReconnectMarginMS, "idle_restart_ms": c.IdleRestartMS,
		"poll_interval_ms": c.PollIntervalMS, "initial_sync_window_ms": c.InitialSyncWindowMS,
		"max_message_bytes": c.MaxMessageBytes,
	} {
		if v < 0 {
			out = append(out, issue(field, "must not be negative"))
		}
	}
	if time.Duration(c.IdleRestartMS)*time.Millisecond > MaxIdleRestart {
		out = append(out, issue("idle_restart_ms", "must be at most 29 minutes"))
	}
	return out
}

func (s Server) validate(at string) []model.ValidationIssue {
	var out []model.ValidationIssue
	if !hostname(s.Host) && net.ParseIP(s.Host) == nil {
		out = append(out, issue(at+".host", "must be a host name or IP address"))
	}
	for field, p := range map[string]int{".tls_port": s.TLSPort, ".starttls_port": s.StartTLSPort, ".plain_port": s.PlainPort} {
		if p < 0 || p > 65535 {
			out = append(out, issue(at+field, "must be a port number"))
		}
	}
	switch {
	case s.TLSPort == 0 && s.StartTLSPort == 0 && s.PlainPort == 0:
		out = append(out, issue(at, "needs tls_port, starttls_port or plain_port"))
	case s.PlainPort != 0 && !s.AllowPlaintextLoopback:
		out = append(out, issue(at+".plain_port", "needs allow_plaintext_loopback"))
	}
	if s.CommandTimeoutMS < 0 || s.DataTimeoutMS < 0 {
		out = append(out, issue(at, "timeouts must not be negative"))
	}
	return out
}

func (s Server) commandTimeout() time.Duration {
	return msOr(s.CommandTimeoutMS, DefaultCommandTimeout)
}

// relay is s as the submission server of package smtp.
func (s Server) relay() smtp.Relay {
	return smtp.Relay{
		Host: s.Host, TLSPort: s.TLSPort, StartTLSPort: s.StartTLSPort, PlainPort: s.PlainPort,
		AllowPlaintextLoopback: s.AllowPlaintextLoopback,
		CommandTimeoutMS:       s.CommandTimeoutMS, DataTimeoutMS: s.DataTimeoutMS,
	}
}

// historyMailboxes are the mailboxes a history read walks, in order: Mailbox
// when none is configured.
func (c Config) historyMailboxes() []string {
	if len(c.HistoryMailboxes) == 0 {
		return []string{c.mailbox()}
	}
	return c.HistoryMailboxes
}

// refusedHistoryNames are the mailbox names, whatever their case or parent,
// a history import never walks: sent, drafts, junk and trash under their
// usual names.
var refusedHistoryNames = []string{
	"sent", "sent items", "sent mail", "sent messages", "drafts", "draft",
	"junk", "junk email", "junk e-mail", "spam", "bulk mail",
	"trash", "deleted items", "deleted messages", "deleted", "bin",
}

// refusedHistoryMailbox reports whether the last element of the mailbox
// path (either separator) is one of refusedHistoryNames.
func refusedHistoryMailbox(name string) bool {
	last := name
	if n := strings.LastIndexAny(name, "/."); n >= 0 {
		last = name[n+1:]
	}
	return slices.ContainsFunc(refusedHistoryNames, func(r string) bool { return strings.EqualFold(r, last) })
}

func (c Config) mailbox() string {
	if c.Mailbox == "" {
		return DefaultMailbox
	}
	return c.Mailbox
}

func (c Config) destination() string {
	if c.Destination == "" {
		return c.Address
	}
	return c.Destination
}

func (c Config) messageIDDomain() string {
	if c.MessageIDDomain != "" {
		return c.MessageIDDomain
	}
	return strings.ToLower(c.Address[strings.LastIndexByte(c.Address, '@')+1:])
}

func (c Config) reconnectMargin() time.Duration {
	return time.Duration(c.ReconnectMarginMS) * time.Millisecond
}

func (c Config) idleRestart() time.Duration { return msOr(c.IdleRestartMS, DefaultIdleRestart) }

func (c Config) pollInterval() time.Duration { return msOr(c.PollIntervalMS, DefaultPollInterval) }

func (c Config) stateSyncInterval() time.Duration {
	return msOr(c.StateSyncIntervalMS, DefaultPollInterval)
}

func (c Config) initialSyncWindow() time.Duration {
	return msOr(c.InitialSyncWindowMS, DefaultInitialSyncWindow)
}

func (c Config) maxMessageBytes() int64 {
	if c.MaxMessageBytes == 0 {
		return DefaultMaxMessageBytes
	}
	return c.MaxMessageBytes
}

func msOr(ms int64, d time.Duration) time.Duration {
	if ms == 0 {
		return d
	}
	return time.Duration(ms) * time.Millisecond
}

// mailboxName reports whether s is a usable mailbox name: one line of
// UTF-8 without control characters, at most 255 bytes.
func mailboxName(s string, emptyOK bool) bool {
	if s == "" {
		return emptyOK
	}
	if len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// hostname reports whether s is a DNS host name: dot-separated labels of
// letters, digits and inner hyphens.
func hostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if !ldh(l[i]) {
				return false
			}
		}
	}
	return true
}

// ldh reports whether c is a letter, digit or hyphen.
func ldh(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}
