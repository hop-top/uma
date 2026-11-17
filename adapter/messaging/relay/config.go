package relay

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration schema.
const (
	ConfigSchema  = "relay.messages.config"
	ConfigVersion = 1
)

// Mediums a channel relays. The medium picks the identity namespace of the
// addresses the relay reports, so one number on SMS, on WhatsApp and on
// iMessage stays three identities until the host links them. An iMessage
// address is a phone number or an Apple ID email address.
const (
	MediumSMS      = "sms"
	MediumWhatsApp = "whatsapp"
	MediumIMessage = "imessage"
)

// Limits on configuration.
const (
	// DefaultMaxBodyBytes is the inbound body limit when none is set.
	DefaultMaxBodyBytes = 64 << 10
	// MaxBodyBytesLimit is the largest inbound body limit a channel may set.
	MaxBodyBytesLimit = 1 << 20

	maxAliases   = 8
	maxNameBytes = 64
)

// Config is a relay.messages channel configuration, carried as the JSON data
// of a ConfigSchema extension. It holds no secret: credentials come from the
// host's credential port, one purpose per direction (PurposeWebhook,
// PurposeCallback).
type Config struct {
	// Medium is MediumSMS, MediumWhatsApp or MediumIMessage. Required.
	Medium string `json:"medium"`
	// Auth is how the relay proves each webhook is its own. Required: there
	// is no unauthenticated mode.
	Auth Auth `json:"auth"`
	// Inbound names the fields of the JSON the relay posts.
	Inbound Inbound `json:"inbound,omitzero"`
	// Callback is where replies go. Without it the channel receives only.
	Callback *Callback `json:"callback,omitempty"`
}

// Auth configures request authentication; it is webhookauth.Config as JSON,
// so a relay already in the field keeps its header names, encodings and
// signed layout. See docs/webhook-auth.md for every field.
type Auth struct {
	// Scheme is bearer, token, hmac or ed25519. Required.
	Scheme         string `json:"scheme"`
	Header         string `json:"header,omitempty"`
	Hash           string `json:"hash,omitempty"`
	Encoding       string `json:"encoding,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	PrefixOptional bool   `json:"prefix_optional,omitempty"`
	Payload        string `json:"payload,omitempty"`
	KeyEncoding    string `json:"key_encoding,omitempty"`
	MinSecretBytes int    `json:"min_secret_bytes,omitempty"`
	// Timestamp and Replay turn on the freshness and replay checks.
	Timestamp AuthTimestamp `json:"timestamp,omitzero"`
	Replay    AuthReplay    `json:"replay,omitzero"`
	// PreviousCredentialRef is the credential reference of the key being
	// retired: while set, a request signed with either key verifies.
	PreviousCredentialRef string `json:"previous_credential_ref,omitempty"`
}

// AuthTimestamp is webhookauth.Timestamp with the tolerance as a duration
// string such as "5m".
type AuthTimestamp struct {
	Header        string `json:"header,omitempty"`
	Format        string `json:"format,omitempty"`
	Tolerance     string `json:"tolerance,omitempty"`
	AllowUnsigned bool   `json:"allow_unsigned,omitempty"`
}

// AuthReplay is webhookauth.Replay with the TTL as a duration string.
type AuthReplay struct {
	Source        string `json:"source,omitempty"`
	Header        string `json:"header,omitempty"`
	TTL           string `json:"ttl,omitempty"`
	AllowUnsigned bool   `json:"allow_unsigned,omitempty"`
}

// Inbound configures what the relay's JSON looks like.
type Inbound struct {
	// Fields are the top-level JSON keys read for each role.
	Fields Fields `json:"fields,omitzero"`
	// GenerateMissingID gives a message that carries no id a random one
	// instead of refusing it. Such a message cannot be recognized when the
	// relay sends it again, so a retry becomes a second message.
	GenerateMissingID bool `json:"generate_missing_id,omitempty"`
	// MaxBodyBytes bounds a request body; zero is DefaultMaxBodyBytes.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`
	// Status turns on delivery status updates on the same route.
	Status Status `json:"status,omitzero"`
}

// Status configures delivery status updates: the relay reports what became
// of a reply (accepted, sent, delivered, read or failed) by posting a JSON
// object to the webhook, authenticated like a message. Off by default: a
// request is then always an incoming message.
type Status struct {
	// Enabled makes a request carrying the status key (Fields.Status) a
	// status update instead of a message.
	Enabled bool `json:"enabled,omitempty"`
	// Fields are the top-level JSON keys read for each role.
	Fields StatusFields `json:"fields,omitzero"`
}

// StatusFields name the JSON keys of a status update, each a list tried in
// order as for Fields. An empty list takes the neutral default.
type StatusFields struct {
	// Status is the key that makes a request a status update.
	Status []string `json:"status,omitempty"`
	// From is the line the reply left from: the address an inbox binds, the
	// message's "to" as the relay reports it.
	From []string `json:"from,omitempty"`
	// OperationID and AttemptID echo the operation_id and attempt_id of the
	// callback being reported on.
	OperationID []string `json:"operation_id,omitempty"`
	AttemptID   []string `json:"attempt_id,omitempty"`
	// MessageID is the relay's id for the message, as the callback answer
	// named it.
	MessageID []string `json:"message_id,omitempty"`
	// Code is the relay's own reason code of a failure.
	Code []string `json:"code,omitempty"`
}

// Fields name the JSON keys of an inbound message, each a list tried in
// order (the first key present wins), so a relay that sends "From" or
// "from" is one configuration. An empty list takes the neutral default.
type Fields struct {
	From      []string `json:"from,omitempty"`
	To        []string `json:"to,omitempty"`
	Body      []string `json:"body,omitempty"`
	MessageID []string `json:"message_id,omitempty"`
	Media     []string `json:"media,omitempty"`
}

// Callback is the relay's reply endpoint. The host's Options.CallbackHosts
// decides which hosts a channel may name; the credential for it is
// PurposeCallback.
type Callback struct {
	// URL is where each reply is posted: https (http only when the host
	// allows it), a host the host lists, no credentials, query or fragment.
	URL string `json:"url"`
	// Auth is how the credential travels: "bearer" (default) as
	// "Authorization: Bearer <secret>", or "header" as the whole value of
	// Header.
	Auth   string `json:"auth,omitempty"`
	Header string `json:"header,omitempty"`
	// MessageID names the keys of the JSON answer that carry the relay's id
	// for the message; empty is ["message_id"]. A lookup answer names the
	// id under the same keys.
	MessageID []string `json:"message_id,omitempty"`
	// LookupURL is where uma asks the relay what became of a reply whose
	// answer was lost (see Instance.Reconcile). Optional: without it
	// reconcile is disabled and a lost answer stays outcome_unknown. The
	// rules of URL apply, and the call carries the same credential, sent
	// the same way.
	LookupURL string `json:"lookup_url,omitempty"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // plain strings, numbers and bools always marshal
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}

// defaults is the neutral field naming.
var defaults = Fields{
	From: []string{"from"}, To: []string{"to"}, Body: []string{"body"},
	MessageID: []string{"message_id"}, Media: []string{"media"},
}

// statusDefaults is the neutral naming of a status update.
var statusDefaults = StatusFields{
	Status: []string{"status"}, From: []string{"from"}, OperationID: []string{"operation_id"},
	AttemptID: []string{"attempt_id"}, MessageID: []string{"message_id"}, Code: []string{"code"},
}

func or(have, def []string) []string {
	if len(have) == 0 {
		return def
	}
	return have
}

// withDefaults fills the roles the configuration left empty.
func (f Fields) withDefaults() Fields {
	return Fields{
		From: or(f.From, defaults.From), To: or(f.To, defaults.To), Body: or(f.Body, defaults.Body),
		MessageID: or(f.MessageID, defaults.MessageID), Media: or(f.Media, defaults.Media),
	}
}

// withDefaults fills the roles the configuration left empty.
func (f StatusFields) withDefaults() StatusFields {
	return StatusFields{
		Status: or(f.Status, statusDefaults.Status), From: or(f.From, statusDefaults.From),
		OperationID: or(f.OperationID, statusDefaults.OperationID), AttemptID: or(f.AttemptID, statusDefaults.AttemptID),
		MessageID: or(f.MessageID, statusDefaults.MessageID), Code: or(f.Code, statusDefaults.Code),
	}
}

// parsed is a validated configuration with its defaults applied.
type parsed struct {
	Config
	fields  Fields
	status  StatusFields
	auth    webhookauth.Config
	maxBody int64
	// previous is the retired credential reference, empty for none.
	previous string
}

// parseConfig decodes and checks a configuration without I/O. opts decides
// which callback hosts are allowed.
func parseConfig(e model.Extension, opts Options) (parsed, []model.ValidationIssue) {
	if e.Name != ConfigSchema || e.Version != ConfigVersion {
		return parsed{}, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch), Message: "want " + ConfigSchema + " version 1",
		}}
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return parsed{}, []model.ValidationIssue{issue("configuration.data", "must be a JSON object of known fields")}
	}
	p := parsed{Config: c, previous: c.Auth.PreviousCredentialRef}
	var out []model.ValidationIssue
	switch c.Medium {
	case MediumSMS, MediumWhatsApp, MediumIMessage:
	default:
		out = append(out, issue("medium", "must be sms, whatsapp or imessage"))
	}
	wc, issues := c.Auth.wire()
	out = append(out, issues...)
	p.auth = wc
	if len(issues) == 0 {
		// New refuses every configuration that could accept an unauthenticated
		// request; it reads no secret here.
		if _, err := webhookauth.New(wc, webhookauth.SecretFunc(nil), webhookauth.Deps{Replay: webhookauth.NewMemoryReplayStore(1, nil)}); err != nil {
			out = append(out, issue("auth", err.Error()))
		}
	}
	out = append(out, p.checkInbound()...)
	if c.Callback != nil {
		out = append(out, checkCallback(*c.Callback, opts)...)
	}
	return p, out
}

// wire converts the JSON form to webhookauth.Config.
func (a Auth) wire() (webhookauth.Config, []model.ValidationIssue) {
	var out []model.ValidationIssue
	wc := webhookauth.Config{
		Scheme: webhookauth.Scheme(a.Scheme), Header: a.Header, Hash: webhookauth.Hash(a.Hash),
		Encoding: webhookauth.Encoding(a.Encoding), Prefix: a.Prefix, PrefixOptional: a.PrefixOptional,
		Payload: a.Payload, KeyEncoding: webhookauth.Encoding(a.KeyEncoding), MinSecretBytes: a.MinSecretBytes,
		Timestamp: webhookauth.Timestamp{
			Header: a.Timestamp.Header, Format: webhookauth.TimestampFormat(a.Timestamp.Format), AllowUnsigned: a.Timestamp.AllowUnsigned,
		},
		Replay: webhookauth.Replay{
			Source: webhookauth.ReplaySource(a.Replay.Source), Header: a.Replay.Header, AllowUnsigned: a.Replay.AllowUnsigned,
		},
	}
	duration := func(field, s string, dst *time.Duration) {
		if s == "" {
			return
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			out = append(out, issue(field, "must be a positive duration such as 5m"))
			return
		}
		*dst = d
	}
	duration("auth.timestamp.tolerance", a.Timestamp.Tolerance, &wc.Timestamp.Tolerance)
	duration("auth.replay.ttl", a.Replay.TTL, &wc.Replay.TTL)
	return wc, out
}

func (p *parsed) checkInbound() []model.ValidationIssue {
	var out []model.ValidationIssue
	in := p.Inbound
	p.fields = in.Fields.withDefaults()
	seen := map[string]bool{}
	clash := false
	for _, role := range []struct {
		field string
		names []string
	}{
		{"from", p.fields.From}, {"to", p.fields.To}, {"body", p.fields.Body},
		{"message_id", p.fields.MessageID}, {"media", p.fields.Media},
	} {
		if len(role.names) > maxAliases {
			out = append(out, issue("inbound.fields."+role.field, "at most 8 names"))
		}
		for _, n := range role.names {
			if !validName(n) {
				out = append(out, issue("inbound.fields."+role.field, "each name is 1 to 64 printable characters"))
				break
			}
			if seen[n] {
				clash = true
			}
			seen[n] = true
		}
	}
	if clash {
		out = append(out, issue("inbound.fields", "a name serves one role, once"))
	}
	out = append(out, p.checkStatus()...)
	switch {
	case in.MaxBodyBytes < 0 || in.MaxBodyBytes > MaxBodyBytesLimit:
		out = append(out, issue("inbound.max_body_bytes", "must be between 0 and 1048576"))
	case in.MaxBodyBytes == 0:
		p.maxBody = DefaultMaxBodyBytes
	default:
		p.maxBody = in.MaxBodyBytes
	}
	return out
}

// checkStatus validates the status field names. They are checked whether or
// not the route is enabled, so switching it on later cannot surface a
// mistake. The status key tells a status update from a message, so no
// message role may use it; the other roles name keys of a different kind of
// request and may share a name with a message role.
func (p *parsed) checkStatus() []model.ValidationIssue {
	var out []model.ValidationIssue
	p.status = p.Inbound.Status.Fields.withDefaults()
	f := p.status
	seen := map[string]bool{}
	clash := false
	for _, role := range []struct {
		field string
		names []string
	}{
		{"status", f.Status}, {"from", f.From}, {"operation_id", f.OperationID},
		{"attempt_id", f.AttemptID}, {"message_id", f.MessageID}, {"code", f.Code},
	} {
		if len(role.names) > maxAliases {
			out = append(out, issue("inbound.status.fields."+role.field, "at most 8 names"))
		}
		for _, n := range role.names {
			if !validName(n) {
				out = append(out, issue("inbound.status.fields."+role.field, "each name is 1 to 64 printable characters"))
				break
			}
			if seen[n] {
				clash = true
			}
			seen[n] = true
		}
	}
	if clash {
		out = append(out, issue("inbound.status.fields", "a name serves one role, once"))
	}
	message := map[string]bool{}
	for _, names := range [][]string{p.fields.From, p.fields.To, p.fields.Body, p.fields.MessageID, p.fields.Media} {
		for _, n := range names {
			message[n] = true
		}
	}
	for _, n := range f.Status {
		if message[n] {
			out = append(out, issue("inbound.status.fields.status", "must not be a name of a message field"))
			break
		}
	}
	return out
}

// validName reports whether n can be a JSON key or an alias: printable, short.
func validName(n string) bool {
	if n == "" || len(n) > maxNameBytes || !utf8.ValidString(n) {
		return false
	}
	for _, r := range n {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// checkCallback judges a callback against the host's options. The URLs are
// never echoed: they may be mistaken for a place to put a secret.
func checkCallback(c Callback, opts Options) []model.ValidationIssue {
	const urlRule = "must be an absolute https URL on a host the host allows, without credentials, query or fragment"
	var out []model.ValidationIssue
	if !opts.callbackAllowed(c.URL) {
		out = append(out, issue("callback.url", urlRule))
	}
	if c.LookupURL != "" && !opts.callbackAllowed(c.LookupURL) {
		out = append(out, issue("callback.lookup_url", urlRule))
	}
	switch c.Auth {
	case "", AuthBearer:
		if c.Header != "" {
			out = append(out, issue("callback.header", "applies to auth \"header\" only"))
		}
	case AuthHeader:
		if !validHeaderName(c.Header) {
			out = append(out, issue("callback.header", "must be a header name the relay reads its credential from"))
		}
	default:
		out = append(out, issue("callback.auth", "must be bearer or header"))
	}
	for _, n := range c.MessageID {
		if !validName(n) {
			out = append(out, issue("callback.message_id", "each name is 1 to 64 printable characters"))
			break
		}
	}
	if len(c.MessageID) > maxAliases {
		out = append(out, issue("callback.message_id", "at most 8 names"))
	}
	return out
}

// Callback credential travel.
const (
	AuthBearer = "bearer"
	AuthHeader = "header"
)

// reservedHeaders the callback credential header must not be, since the
// adapter sets them itself or the transport owns them.
var reservedHeaders = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "transfer-encoding": true, "connection": true,
	"idempotency-key": true, "upgrade": true, "te": true, "trailer": true, "accept": true,
}

func validHeaderName(n string) bool {
	if n == "" || len(n) > 128 || reservedHeaders[strings.ToLower(n)] {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
		if !ok {
			return false
		}
	}
	return true
}

// callbackAllowed reports whether raw is a callback URL this host allows.
func (o Options) callbackAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && o.AllowHTTP:
	default:
		return false
	}
	host := strings.ToLower(u.Host)
	for _, h := range o.CallbackHosts {
		if strings.ToLower(h) == host {
			return true
		}
	}
	return false
}
