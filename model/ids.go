package model

import (
	"unicode"
	"unicode/utf8"
)

// Byte limits for opaque values (v0).
const (
	// maxTokenBytes bounds host and caller tokens (tenant, actor, operation,
	// correlation), identity source IDs and engine-issued revisions.
	maxTokenBytes = 255
	// maxExternalIDBytes bounds provider IDs in ExternalRef.
	maxExternalIDBytes = 1024
	// maxKeyBytes bounds registry-key shaped names.
	maxKeyBytes = 253
)

// TenantID is a host-defined tenant key from trusted authentication.
type TenantID string

// ActorID is a host-defined actor key from trusted authentication.
type ActorID string

// ID is an internal resource identifier: canonical RFC 9562 UUID text. Callers
// must treat it as opaque: no parsing, no time extraction, no ordering.
type ID string

// OperationID is the caller-chosen replay key of a command.
type OperationID string

// CorrelationID is a caller-chosen diagnostic token; it never affects replay.
type CorrelationID string

type Cursor string
type Revision string
type TypeKey string
type Capability string

// ParseID returns s as an ID when it is canonical RFC 9562 UUID text: 36
// bytes, lowercase hex, hyphens at offsets 8, 13, 18 and 23, version 1-8 and
// variant 10xx. The nil and max UUIDs fail the version check. s is never
// trimmed or case-folded.
func ParseID(s string) (ID, error) {
	if err := ID(s).Validate(); err != nil {
		return "", err
	}
	return ID(s), nil
}

// Validate applies the ParseID rules.
func (id ID) Validate() error { return checkID("id", id) }

func checkID(field string, id ID) error {
	if p := idProblem(string(id)); p != "" {
		return invalidField(field, p)
	}
	return nil
}

func idProblem(s string) string {
	if s == "" {
		return "required"
	}
	if len(s) != 36 {
		return "must be canonical lowercase UUID text"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "must be canonical lowercase UUID text"
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return "must be canonical lowercase UUID text"
			}
		}
	}
	if s[14] < '1' || s[14] > '8' {
		return "must carry an RFC 9562 version"
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
	default:
		return "must carry the RFC 9562 variant"
	}
	return ""
}

// ParseTenantID returns s as a TenantID when it satisfies the opaque-token rule:
// 1-255 bytes of valid UTF-8, no control characters and no leading or trailing
// space. Tokens compare byte-exact and are never rewritten.
func ParseTenantID(s string) (TenantID, error) {
	if err := TenantID(s).Validate(); err != nil {
		return "", err
	}
	return TenantID(s), nil
}

// ParseActorID applies the ParseTenantID rules to an actor key.
func ParseActorID(s string) (ActorID, error) {
	if err := ActorID(s).Validate(); err != nil {
		return "", err
	}
	return ActorID(s), nil
}

// Validate applies the opaque-token rule.
func (t TenantID) Validate() error { return checkToken("tenant", string(t)) }

// Validate applies the opaque-token rule.
func (a ActorID) Validate() error { return checkToken("actor", string(a)) }

// Validate applies the opaque-token rule.
func (o OperationID) Validate() error { return checkToken("operation", string(o)) }

// Validate applies the opaque-token rule.
func (c CorrelationID) Validate() error { return checkToken("correlation", string(c)) }

// Validate checks tenant and actor tokens. It does not authenticate anyone.
func (p Principal) Validate() error {
	return firstError(p.Tenant.Validate(), p.Actor.Validate())
}

func checkToken(field, s string) error {
	if p := tokenProblem(s); p != "" {
		return invalidField(field, p)
	}
	return nil
}

func checkOptionalToken(field, s string) error {
	if s == "" {
		return nil
	}
	return checkToken(field, s)
}

func tokenProblem(s string) string {
	switch {
	case s == "":
		return "required"
	case len(s) > maxTokenBytes:
		return "exceeds 255 bytes"
	case !utf8.ValidString(s):
		return "must be valid UTF-8"
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "must not contain control characters"
		}
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return "must not have leading or trailing space"
	}
	return ""
}

// rawProblem checks a byte-preserving external value: non-empty, at most max
// bytes, valid UTF-8, no NUL.
func rawProblem(s string, max int) string {
	switch {
	case s == "":
		return "required"
	case len(s) > max:
		return "exceeds byte limit"
	case !utf8.ValidString(s):
		return "must be valid UTF-8"
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return "must not contain NUL"
		}
	}
	return ""
}
