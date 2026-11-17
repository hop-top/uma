package smtp

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
)

// Names this adapter owns.
const (
	// ConfigSchema and ConfigVersion name the channel configuration.
	ConfigSchema  = "smtp.mail.config"
	ConfigVersion = 1
	// Namespace is the identity and route namespace: a normalized mailbox
	// address (model.NormalizeEmail).
	Namespace = "smtp.mail.address"
	// RouteKind is the kind of sender and route references.
	RouteKind = "address"
	// MessageSchema and MessageVersion name the envelope payload recorded as
	// message metadata: SMTP envelope, header fields and sender evidence.
	MessageSchema  = "smtp.mail.message"
	MessageVersion = 1
	// PartsSchema and PartsVersion name the structured content part that
	// describes non-text MIME parts.
	PartsSchema  = "smtp.mail.parts"
	PartsVersion = 1
	// PurposeRelay is the credential purpose of relay submission.
	PurposeRelay = "smtp.relay"
)

// Configuration defaults.
const (
	DefaultMaxMessageBytes = 25 << 20
	DefaultMaxRecipients   = 50
	DefaultCommandTimeout  = 5 * time.Minute
	DefaultDataTimeout     = 10 * time.Minute
)

// Config is the channel configuration, the JSON data of a ConfigSchema
// extension.
type Config struct {
	// Hostname is the receiver's greeting and EHLO name and the
	// Authentication-Results authserv-id.
	Hostname string `json:"hostname"`
	// Recipients lists the addresses the receiver accepts, each mapped to a
	// destination. Others are refused at RCPT.
	Recipients []Recipient `json:"recipients"`
	// MaxMessageBytes and MaxRecipients bound one inbound message; zero
	// takes the default.
	MaxMessageBytes int64 `json:"max_message_bytes,omitempty"`
	MaxRecipients   int   `json:"max_recipients,omitempty"`
	// From is the bare sender address of outbound mail, FromName its
	// optional display name, Subject the optional subject.
	From     string `json:"from"`
	FromName string `json:"from_name,omitempty"`
	Subject  string `json:"subject,omitempty"`
	// MessageIDDomain is the right-hand side of outbound Message-IDs; empty
	// takes From's domain.
	MessageIDDomain string `json:"message_id_domain,omitempty"`
	Relay           Relay  `json:"relay"`
}

// Recipient maps one accepted address to a destination.
type Recipient struct {
	Address     string `json:"address"`
	Destination string `json:"destination"`
}

// Relay is the submission server of outbound mail. At least one port is set;
// implicit TLS is tried first, STARTTLS when it is unset or refuses the
// connection. PlainPort needs AllowPlaintextLoopback and is used only when
// no TLS port is set, and only to a loopback address.
type Relay struct {
	Host                   string `json:"host"`
	TLSPort                int    `json:"tls_port,omitempty"`
	StartTLSPort           int    `json:"starttls_port,omitempty"`
	PlainPort              int    `json:"plain_port,omitempty"`
	AllowPlaintextLoopback bool   `json:"allow_plaintext_loopback,omitempty"`
	// CommandTimeoutMS bounds each command reply; DataTimeoutMS the reply to
	// the end of DATA. Zero takes the default.
	CommandTimeoutMS int64 `json:"command_timeout_ms,omitempty"`
	DataTimeoutMS    int64 `json:"data_timeout_ms,omitempty"`
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
	if !hostname(c.Hostname) {
		out = append(out, issue("hostname", "must be a host name"))
	}
	if len(c.Recipients) == 0 {
		out = append(out, issue("recipients", "required"))
	}
	seen := map[string]bool{}
	for i, r := range c.Recipients {
		at := "recipients[" + strconv.Itoa(i) + "]"
		norm, err := model.NormalizeEmail(r.Address)
		switch {
		case err != nil:
			out = append(out, issue(at+".address", "must be a bare address"))
		case seen[norm]:
			out = append(out, issue(at+".address", "duplicate address"))
		}
		seen[norm] = true
		if strings.TrimSpace(r.Destination) == "" || r.Destination != strings.TrimSpace(r.Destination) {
			out = append(out, issue(at+".destination", "required, without surrounding space"))
		}
	}
	if c.MaxMessageBytes < 0 {
		out = append(out, issue("max_message_bytes", "must not be negative"))
	}
	if c.MaxRecipients < 0 {
		out = append(out, issue("max_recipients", "must not be negative"))
	}
	if _, err := model.NormalizeEmail(c.From); err != nil {
		out = append(out, issue("from", "must be a bare address"))
	}
	if strings.ContainsAny(c.FromName, "\r\n") {
		out = append(out, issue("from_name", "must be one line"))
	}
	if strings.ContainsAny(c.Subject, "\r\n") {
		out = append(out, issue("subject", "must be one line"))
	}
	if c.MessageIDDomain != "" && !hostname(c.MessageIDDomain) {
		out = append(out, issue("message_id_domain", "must be a host name"))
	}
	return append(out, c.Relay.validate()...)
}

func (r Relay) validate() []model.ValidationIssue {
	var out []model.ValidationIssue
	if !hostname(r.Host) && !ipLiteral(r.Host) {
		out = append(out, issue("relay.host", "must be a host name or IP address"))
	}
	for field, p := range map[string]int{"relay.tls_port": r.TLSPort, "relay.starttls_port": r.StartTLSPort, "relay.plain_port": r.PlainPort} {
		if p < 0 || p > 65535 {
			out = append(out, issue(field, "must be a port number"))
		}
	}
	switch {
	case r.TLSPort == 0 && r.StartTLSPort == 0 && r.PlainPort == 0:
		out = append(out, issue("relay", "needs tls_port, starttls_port or plain_port"))
	case r.PlainPort != 0 && !r.AllowPlaintextLoopback:
		out = append(out, issue("relay.plain_port", "needs allow_plaintext_loopback"))
	}
	if r.CommandTimeoutMS < 0 || r.DataTimeoutMS < 0 {
		out = append(out, issue("relay", "timeouts must not be negative"))
	}
	return out
}

func (c Config) maxMessageBytes() int64 {
	if c.MaxMessageBytes == 0 {
		return DefaultMaxMessageBytes
	}
	return c.MaxMessageBytes
}

func (c Config) maxRecipients() int {
	if c.MaxRecipients == 0 {
		return DefaultMaxRecipients
	}
	return c.MaxRecipients
}

func (r Relay) commandTimeout() time.Duration {
	if r.CommandTimeoutMS == 0 {
		return DefaultCommandTimeout
	}
	return time.Duration(r.CommandTimeoutMS) * time.Millisecond
}

func (r Relay) dataTimeout() time.Duration {
	if r.DataTimeoutMS == 0 {
		return DefaultDataTimeout
	}
	return time.Duration(r.DataTimeoutMS) * time.Millisecond
}

// destination returns the destination of an envelope recipient; the domain
// compares case-insensitively, the local part byte-exact.
func (c Config) destination(rcpt string) (string, bool) {
	norm, err := model.NormalizeEmail(rcpt)
	if err != nil {
		return "", false
	}
	for _, r := range c.Recipients {
		if n, err := model.NormalizeEmail(r.Address); err == nil && n == norm {
			return r.Destination, true
		}
	}
	return "", false
}

func (c Config) messageIDDomain() string {
	if c.MessageIDDomain != "" {
		return c.MessageIDDomain
	}
	return strings.ToLower(c.From[strings.LastIndexByte(c.From, '@')+1:])
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
