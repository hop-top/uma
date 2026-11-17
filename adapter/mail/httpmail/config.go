package httpmail

import (
	"bytes"
	"encoding/json"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
)

// Names this adapter owns.
const (
	// ConfigSchema and ConfigVersion name the channel configuration.
	ConfigSchema  = "http.mail.config"
	ConfigVersion = 1
	// Namespace is the identity and route namespace: a normalized mailbox
	// address (model.NormalizeEmail).
	Namespace = "http.mail.address"
	// RouteKind is the kind of sender references.
	RouteKind = "address"
	// MessageSchema and MessageVersion name the envelope payload recorded as
	// message metadata: how the message arrived, header fields and sender
	// evidence.
	MessageSchema  = "http.mail.message"
	MessageVersion = 1
	// PartsSchema and PartsVersion name the structured content part that
	// describes attachments, staged or not. Bytes are never part of it:
	// staged ones are separate attachment parts.
	PartsSchema  = "http.mail.parts"
	PartsVersion = 1
	// PurposeWebhook is the credential purpose of request authentication:
	// the shared token, HMAC key or Ed25519 public key.
	PurposeWebhook = "http.mail.webhook"
)

// Body formats a channel accepts, the value of Config.Formats.
const (
	// FormatJSON is one JSON object per message, content type
	// application/json.
	FormatJSON = "json"
	// FormatRFC822 is the message itself, content type message/rfc822.
	FormatRFC822 = "rfc822"
)

// Defaults and bounds.
const (
	// DefaultMaxBodyBytes is the request body limit when the configuration
	// names none. The engine bounds a webhook body to its own limit as well.
	DefaultMaxBodyBytes = 1 << 20
	maxRecipients       = 100
	maxFieldName        = 128
)

// Config is the channel configuration, the JSON data of a ConfigSchema
// extension. It holds no secret: the shared token, key or public key comes
// from the host's credential port.
type Config struct {
	// Recipients lists the addresses the channel accepts, each mapped to a
	// destination. A message to none of them is refused.
	Recipients []Recipient `json:"recipients"`
	// Auth is how a request proves it comes from the relay; see Auth.
	Auth Auth `json:"auth"`
	// Formats lists the body formats accepted, by request content type:
	// FormatJSON (application/json) and FormatRFC822 (message/rfc822). Empty
	// takes FormatJSON alone.
	Formats []string `json:"formats,omitempty"`
	// JSONFields names the members of a JSON body; empty names take the
	// defaults.
	JSONFields JSONFields `json:"json_fields,omitempty"`
	// RecipientHeader names a request header carrying the SMTP envelope
	// recipients of a FormatRFC822 body (a comma-separated address list).
	// Empty reads the To and Cc fields of the message.
	RecipientHeader string `json:"recipient_header,omitempty"`
	// Evidence declares which sender evidence the channel trusts; see
	// Evidence. The zero value trusts none: every message is unverified.
	Evidence Evidence `json:"evidence,omitempty"`
	// MaxBodyBytes bounds the request body; zero takes DefaultMaxBodyBytes.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`
}

// Recipient maps one accepted address to a destination.
type Recipient struct {
	Address     string `json:"address"`
	Destination string `json:"destination"`
}

// Auth is the request authentication, a webhookauth.Config in JSON form.
// Scheme is required and never read from the request. Every other field
// takes webhookauth's neutral default when empty; see its documentation for
// the schemes, encodings, payload layouts and the timestamp and replay
// checks.
type Auth struct {
	Scheme         string `json:"scheme"`
	Header         string `json:"header,omitempty"`
	Hash           string `json:"hash,omitempty"`
	Encoding       string `json:"encoding,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	PrefixOptional bool   `json:"prefix_optional,omitempty"`
	Payload        string `json:"payload,omitempty"`
	KeyEncoding    string `json:"key_encoding,omitempty"`
	MinSecretBytes int    `json:"min_secret_bytes,omitempty"`
	// Timestamp turns on the freshness check.
	Timestamp *AuthTimestamp `json:"timestamp,omitempty"`
	// Replay turns on replay rejection. It needs Options.ReplayStore.
	Replay *AuthReplay `json:"replay,omitempty"`
}

// AuthTimestamp is webhookauth.Timestamp.
type AuthTimestamp struct {
	Header           string `json:"header"`
	Format           string `json:"format,omitempty"`
	ToleranceSeconds int64  `json:"tolerance_seconds,omitempty"`
	AllowUnsigned    bool   `json:"allow_unsigned,omitempty"`
}

// AuthReplay is webhookauth.Replay.
type AuthReplay struct {
	Source        string `json:"source"`
	Header        string `json:"header,omitempty"`
	TTLSeconds    int64  `json:"ttl_seconds,omitempty"`
	AllowUnsigned bool   `json:"allow_unsigned,omitempty"`
}

// JSONFields names the members of a JSON body and of its attachment
// entries. The defaults are the neutral names {from, to, subject, body};
// another sender's names are configuration. Only top-level members are read.
type JSONFields struct {
	// From is the sender: an address, or "Name <address>".
	From string `json:"from,omitempty"`
	// To is the recipients: an address list as a string, or an array of
	// such strings.
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	// Body is the plain text of the message.
	Body string `json:"body,omitempty"`
	// MessageID, InReplyTo and References carry the threading headers
	// (RFC 5322 msg-ids). A value that is no msg-id threads nothing.
	MessageID  string `json:"message_id,omitempty"`
	InReplyTo  string `json:"in_reply_to,omitempty"`
	References string `json:"references,omitempty"`
	// Date is when the message was written, RFC 3339 or RFC 5322.
	Date string `json:"date,omitempty"`
	// Attachments is an array of attachment descriptions; their members
	// are named by AttachmentFields.
	Attachments      string           `json:"attachments,omitempty"`
	AttachmentFields AttachmentFields `json:"attachment_fields,omitempty"`
}

// AttachmentFields names the members of one attachment description.
type AttachmentFields struct {
	// Type is the sender's own kind ("image", "document", ...).
	Type string `json:"type,omitempty"`
	// URL is where the sender says the bytes are. It is recorded as a claim
	// and never fetched.
	URL       string `json:"url,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes string `json:"size_bytes,omitempty"`
	Name      string `json:"name,omitempty"`
	// Content is the member holding the attachment's bytes, base64-encoded
	// (Postmark's Content). Bytes within the staging bounds are staged
	// through the engine's attachment sink; a value that is no base64, or
	// is beyond the bounds, leaves the attachment described only.
	Content string `json:"content,omitempty"`
}

// Evidence is the channel's trust declaration for sender evidence.
//
// Without TrustedAuthservID nothing a request carries counts as verified:
// the Authentication-Results, Received-SPF and DKIM-Signature a sender
// supplies are recorded as claims and the source assurance is "unverified".
// With it, the operator declares that requests passing Auth come from a
// relay that evaluated SPF, DKIM and DMARC itself and stamped its results
// under that authserv-id (RFC 8601): an MTA hook running next to the MX, or
// a hosted service whose signed webhook vouches for what it stamped. Only
// an Authentication-Results value naming exactly that id is read, and only
// then do its results become evidence.
type Evidence struct {
	// TrustedAuthservID is the authserv-id the channel trusts.
	TrustedAuthservID string `json:"trusted_authserv_id,omitempty"`
	// AuthenticationResultsField names the JSON member carrying an
	// Authentication-Results value, for a FormatJSON body. A message in
	// FormatRFC822 carries it as a header field.
	AuthenticationResultsField string `json:"authentication_results_field,omitempty"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // plain struct: cannot fail
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// ParseConfig decodes a channel configuration and checks it, with the
// defaults applied to the returned Config. It makes no host-dependent check:
// Factory.Validate adds those.
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
	c = c.withDefaults()
	return c, c.validate()
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}

func (c Config) withDefaults() Config {
	if len(c.Formats) == 0 {
		c.Formats = []string{FormatJSON}
	}
	f := &c.JSONFields
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&f.From, "from")
	def(&f.To, "to")
	def(&f.Subject, "subject")
	def(&f.Body, "body")
	def(&f.MessageID, "message_id")
	def(&f.InReplyTo, "in_reply_to")
	def(&f.References, "references")
	def(&f.Date, "date")
	def(&f.Attachments, "attachments")
	def(&f.AttachmentFields.Type, "type")
	def(&f.AttachmentFields.URL, "url")
	def(&f.AttachmentFields.MimeType, "mime_type")
	def(&f.AttachmentFields.SizeBytes, "size_bytes")
	def(&f.AttachmentFields.Name, "name")
	def(&f.AttachmentFields.Content, "content")
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return c
}

func (c Config) accepts(format string) bool {
	for _, f := range c.Formats {
		if f == format {
			return true
		}
	}
	return false
}

func (c Config) validate() []model.ValidationIssue {
	var out []model.ValidationIssue
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
	out = append(out, c.validateFormats()...)
	if c.MaxBodyBytes < 0 {
		out = append(out, issue("max_body_bytes", "must not be negative"))
	}
	if c.RecipientHeader != "" && !headerName(c.RecipientHeader) {
		out = append(out, issue("recipient_header", "must be a header field name"))
	}
	if c.RecipientHeader != "" && !c.accepts(FormatRFC822) {
		out = append(out, issue("recipient_header", "applies to the "+FormatRFC822+" format, which is not accepted"))
	}
	out = append(out, c.validateEvidence()...)
	return append(out, c.validateAuth()...)
}

func (c Config) validateFormats() []model.ValidationIssue {
	var out []model.ValidationIssue
	seen := map[string]bool{}
	for i, f := range c.Formats {
		at := "formats[" + strconv.Itoa(i) + "]"
		switch {
		case f != FormatJSON && f != FormatRFC822:
			out = append(out, issue(at, "must be "+FormatJSON+" or "+FormatRFC822))
		case seen[f]:
			out = append(out, issue(at, "duplicate format"))
		}
		seen[f] = true
	}
	if !c.accepts(FormatJSON) {
		return out
	}
	// Two roles must not read one member: a sender that names "from" for
	// both the author and the recipient is misconfigured, not ambiguous.
	f := c.JSONFields
	roles := []struct{ field, name string }{
		{"json_fields.from", f.From}, {"json_fields.to", f.To}, {"json_fields.subject", f.Subject},
		{"json_fields.body", f.Body}, {"json_fields.message_id", f.MessageID},
		{"json_fields.in_reply_to", f.InReplyTo}, {"json_fields.references", f.References},
		{"json_fields.date", f.Date}, {"json_fields.attachments", f.Attachments},
	}
	if c.Evidence.AuthenticationResultsField != "" {
		roles = append(roles, struct{ field, name string }{"evidence.authentication_results_field", c.Evidence.AuthenticationResultsField})
	}
	names := map[string]string{}
	for _, r := range roles {
		if !memberName(r.name) {
			out = append(out, issue(r.field, "must be a member name of at most 128 characters, without control characters"))
			continue
		}
		if prev, dup := names[r.name]; dup {
			out = append(out, issue(r.field, "reads the same member as "+prev))
		}
		names[r.name] = r.field
	}
	af := f.AttachmentFields
	entry := map[string]string{}
	for _, r := range []struct{ field, name string }{
		{"json_fields.attachment_fields.type", af.Type}, {"json_fields.attachment_fields.url", af.URL},
		{"json_fields.attachment_fields.mime_type", af.MimeType}, {"json_fields.attachment_fields.size_bytes", af.SizeBytes},
		{"json_fields.attachment_fields.name", af.Name}, {"json_fields.attachment_fields.content", af.Content},
	} {
		if !memberName(r.name) {
			out = append(out, issue(r.field, "must be a member name of at most 128 characters, without control characters"))
			continue
		}
		if prev, dup := entry[r.name]; dup {
			out = append(out, issue(r.field, "reads the same member as "+prev))
		}
		entry[r.name] = r.field
	}
	return out
}

func (c Config) validateEvidence() []model.ValidationIssue {
	var out []model.ValidationIssue
	e := c.Evidence
	if e.TrustedAuthservID != "" && !authservID(e.TrustedAuthservID) {
		out = append(out, issue("evidence.trusted_authserv_id", "must be one token: no space, quote, semicolon or parenthesis"))
	}
	if e.AuthenticationResultsField != "" && e.TrustedAuthservID == "" {
		out = append(out, issue("evidence.authentication_results_field", "needs trusted_authserv_id: without a declared trust the value would only be a claim"))
	}
	if e.AuthenticationResultsField != "" && !c.accepts(FormatJSON) {
		out = append(out, issue("evidence.authentication_results_field", "applies to the "+FormatJSON+" format, which is not accepted"))
	}
	return out
}

// webhookConfig is the webhookauth configuration of a validated Auth.
func (c Config) webhookConfig() webhookauth.Config {
	a := c.Auth
	w := webhookauth.Config{
		Scheme:         webhookauth.Scheme(a.Scheme),
		Header:         a.Header,
		Hash:           webhookauth.Hash(a.Hash),
		Encoding:       webhookauth.Encoding(a.Encoding),
		Prefix:         a.Prefix,
		PrefixOptional: a.PrefixOptional,
		Payload:        a.Payload,
		KeyEncoding:    webhookauth.Encoding(a.KeyEncoding),
		MinSecretBytes: a.MinSecretBytes,
		MaxBodyBytes:   c.MaxBodyBytes,
	}
	if t := a.Timestamp; t != nil {
		w.Timestamp = webhookauth.Timestamp{
			Header: t.Header, Format: webhookauth.TimestampFormat(t.Format),
			Tolerance: time.Duration(t.ToleranceSeconds) * time.Second, AllowUnsigned: t.AllowUnsigned,
		}
	}
	if r := a.Replay; r != nil {
		w.Replay = webhookauth.Replay{
			Source: webhookauth.ReplaySource(r.Source), Header: r.Header,
			TTL: time.Duration(r.TTLSeconds) * time.Second, AllowUnsigned: r.AllowUnsigned,
		}
	}
	return w
}

// validateAuth lets webhookauth judge the authentication: it refuses every
// configuration that could accept an unauthenticated request.
func (c Config) validateAuth() []model.ValidationIssue {
	var out []model.ValidationIssue
	if t := c.Auth.Timestamp; t != nil && t.ToleranceSeconds < 0 {
		out = append(out, issue("auth.timestamp.tolerance_seconds", "must not be negative"))
	}
	if r := c.Auth.Replay; r != nil && r.TTLSeconds < 0 {
		out = append(out, issue("auth.replay.ttl_seconds", "must not be negative"))
	}
	if len(out) > 0 {
		return out
	}
	// A throwaway store stands in for the host's: only the shape is judged.
	_, err := webhookauth.New(c.webhookConfig(), webhookauth.SecretFunc(nil), webhookauth.Deps{Replay: webhookauth.NewMemoryReplayStore(1, nil)})
	if err != nil {
		return []model.ValidationIssue{issue("auth", strings.TrimPrefix(err.Error(), "webhookauth: "))}
	}
	return nil
}

// headerName reports whether s is an HTTP field name: a non-empty token.
func headerName(s string) bool {
	if s == "" || len(s) > maxFieldName {
		return false
	}
	return textproto.TrimString(s) == s && !strings.ContainsAny(s, " \t\r\n:()<>@,;\\\"/[]?={}")
}

// memberName reports whether s can name a JSON member: non-empty, bounded
// and free of control characters.
func memberName(s string) bool {
	if s == "" || len(s) > maxFieldName {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// authservID reports whether s can be an authserv-id: one run of printable
// ASCII without the characters that delimit an Authentication-Results field.
func authservID(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || strings.IndexByte(`;"()\<>`, c) >= 0 {
			return false
		}
	}
	return true
}

// destination returns the destination of a recipient address; the domain
// compares case-insensitively, the local part byte-exact.
func (c Config) destination(addr string) (string, bool) {
	norm, err := model.NormalizeEmail(addr)
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
