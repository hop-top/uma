package model

import (
	"net/mail"
	"regexp"
	"strings"
)

// identityNamespacePattern is the shape of IdentityKey and ExternalRef
// namespaces, e.g. slack.user, irc.account, smtp.address.
var identityNamespacePattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9_-]+)+$`)

func checkNamespace(field, ns string) error {
	if ns == "" {
		return invalidField(field, "required")
	}
	if len(ns) > maxTokenBytes || !identityNamespacePattern.MatchString(ns) {
		return invalidField(field, "must be lowercase dot-separated segments")
	}
	return nil
}

func checkRaw(field, s string, max int) error {
	if p := rawProblem(s, max); p != "" {
		return invalidField(field, p)
	}
	return nil
}

// NewIdentityKey returns a validated identity lookup and claim key. sourceID is
// the provider's opaque identity ID, kept byte-exact: never trimmed,
// case-folded or normalized.
func NewIdentityKey(in InboxRef, namespace, sourceID string) (IdentityKey, error) {
	return newValid(IdentityKey{Inbox: in, Namespace: namespace, SourceID: sourceID})
}

// Validate checks the inbox, the namespace shape and the source ID: 1-255
// bytes of valid UTF-8 without NUL.
func (k IdentityKey) Validate() error {
	return firstError(
		k.Inbox.validate("inbox", false),
		checkNamespace("namespace", k.Namespace),
		checkRaw("source_id", k.SourceID, maxTokenBytes),
	)
}

// Scope returns the owning tenant.
func (k IdentityKey) Scope() TenantID { return k.Inbox.Tenant }

// NewExternalRef returns a validated provider reference. id and parentID stay
// byte-exact; equality is byte equality within (channel, namespace, kind).
func NewExternalRef(ch ChannelRef, namespace, kind, id, parentID string) (ExternalRef, error) {
	return newValid(ExternalRef{Channel: ch, Namespace: namespace, Kind: kind, ID: id, ParentID: parentID})
}

// Validate checks the channel, the namespace and kind shapes, and the provider
// IDs: 1-1024 bytes of valid UTF-8 without NUL; ParentID may be empty.
func (r ExternalRef) Validate() error { return r.validate("") }

func (r ExternalRef) validate(path string) error {
	var parent error
	if r.ParentID != "" {
		parent = checkRaw(fieldPath(path, "parent_id"), r.ParentID, maxExternalIDBytes)
	}
	var kind error
	if r.Kind == "" {
		kind = invalidField(fieldPath(path, "kind"), "required")
	} else if !isRegistryKey(r.Kind, 1) {
		kind = invalidField(fieldPath(path, "kind"), "must be lowercase dot-separated segments")
	}
	return firstError(
		r.Channel.validate(fieldPath(path, "channel"), false),
		checkNamespace(fieldPath(path, "namespace"), r.Namespace),
		kind,
		checkRaw(fieldPath(path, "id"), r.ID, maxExternalIDBytes),
		parent,
	)
}

// Scope returns the owning tenant.
func (r ExternalRef) Scope() TenantID { return r.Channel.Tenant }

// IsZero reports whether r is unset.
func (r ExternalRef) IsZero() bool { return r == ExternalRef{} }

// AttributeKind names a normalized contact attribute.
type AttributeKind string

const (
	AttributeEmail AttributeKind = "email"
	AttributePhone AttributeKind = "phone"
	// AttributeWhatsAppUserID is a WhatsApp business-scoped user ID: two
	// uppercase letters, a dot, then 1 to 128 letters and digits, with
	// "ENT." after the dot for a parent ID (CA.0000000000000002,
	// US.ENT.11815799212886844830). It is unique per user and business
	// portfolio, and WhatsApp issues a new one when the user changes number.
	// Case is kept as WhatsApp sends it.
	AttributeWhatsAppUserID AttributeKind = "whatsapp_user_id"
)

// Attribute is a normalized, optional, non-unique identity attribute. It is a
// lookup hint and verified-attribute link evidence input only: never part of
// IdentityKey, never authorization, never a selection key.
type Attribute struct {
	Kind AttributeKind `json:"kind"`
	// Value is normalized; see NormalizeEmail and NormalizePhone.
	Value string `json:"value"`
	// Verified is the provider's verification assertion, set by the adapter.
	Verified bool `json:"verified"`
}

// Validate checks that Kind is known and Value is already normalized.
func (a Attribute) Validate() error {
	var norm string
	var err error
	switch a.Kind {
	case AttributeEmail:
		norm, err = NormalizeEmail(a.Value)
	case AttributePhone:
		norm, err = NormalizePhone(a.Value)
	case AttributeWhatsAppUserID:
		norm, err = NormalizeWhatsAppUserID(a.Value)
	default:
		return invalidField("kind", "must be email, phone or whatsapp_user_id")
	}
	if err != nil {
		return invalidField("value", "must be a valid "+string(a.Kind)+" attribute")
	}
	if norm != a.Value {
		return invalidField("value", "must be normalized")
	}
	return nil
}

// NormalizeEmail returns raw with its domain lowercased. raw must be a bare
// RFC 5322 address: no display name, angle brackets, comments, quoting changes
// or surrounding space. The local part stays byte-exact (it may be
// case-sensitive); non-ASCII domains are lowercased without IDNA conversion.
func NormalizeEmail(raw string) (string, error) {
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Name != "" || addr.Address != raw {
		return "", invalidField("email", "must be a bare address")
	}
	at := strings.LastIndexByte(raw, '@')
	return raw[:at] + strings.ToLower(raw[at:]), nil
}

// NormalizePhone strips ASCII space, '-', '.', '(' and ')' and requires E.164:
// '+', a non-zero digit, then 1-14 digits. National formats are rejected;
// adapters convert them first.
func NormalizePhone(raw string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case ' ', '-', '.', '(', ')':
		default:
			b.WriteByte(c)
		}
	}
	s := b.String()
	if !isE164(s) {
		return "", invalidField("phone", "must be an E.164 number")
	}
	return s, nil
}

// NormalizeWhatsAppUserID returns raw when it is a WhatsApp business-scoped
// user ID (see AttributeWhatsAppUserID). The value is not rewritten: the
// "whatsapp:" address prefix and any other decoration are refused, an
// adapter strips them first.
func NormalizeWhatsAppUserID(raw string) (string, error) {
	rest := raw
	if len(rest) < 4 || !isUpperAlpha(rest[0]) || !isUpperAlpha(rest[1]) || rest[2] != '.' {
		return "", invalidField("whatsapp_user_id", "must be a WhatsApp user ID")
	}
	rest = rest[3:]
	rest = strings.TrimPrefix(rest, "ENT.")
	if len(rest) < 1 || len(rest) > maxWhatsAppUserIDBody {
		return "", invalidField("whatsapp_user_id", "must be a WhatsApp user ID")
	}
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; !isUpperAlpha(c) && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", invalidField("whatsapp_user_id", "must be a WhatsApp user ID")
		}
	}
	return raw, nil
}

// maxWhatsAppUserIDBody bounds the letters and digits after the country code
// (and the "ENT." of a parent ID).
const maxWhatsAppUserIDBody = 128

func isUpperAlpha(c byte) bool { return c >= 'A' && c <= 'Z' }

func isE164(s string) bool {
	if len(s) < 3 || len(s) > 16 || s[0] != '+' || s[1] < '1' || s[1] > '9' {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
