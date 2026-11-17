package ports

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"hop.top/uma/model"
)

// CredentialKind names a credential shape. Values are stable wire strings.
type CredentialKind string

const (
	// KindSecret is opaque bytes: API key, auth token, signing secret.
	KindSecret CredentialKind = "secret"
	// KindPassword is a user and password: account, app, app-specific or
	// SMTP-token password.
	KindPassword CredentialKind = "password"
	// KindOAuth2 is a bearer access token. The host refreshes and rotates it.
	KindOAuth2 CredentialKind = "oauth2"
	// KindClientCert is a TLS client certificate (mutual TLS, SASL EXTERNAL).
	KindClientCert CredentialKind = "client_cert"
	// KindKerberos is a Kerberos principal with a keytab or a credential
	// cache (GSSAPI).
	KindKerberos CredentialKind = "kerberos"
)

// Credential is one resolved credential. The set of implementations is
// closed: Secret, Password, OAuth2, ClientCert and Kerberos. Adapters switch
// on the concrete type and fail with model.Unsupported for kinds they cannot
// use.
//
// Every implementation formats as "[redacted]" through fmt (any verb),
// encoding/json, encoding.TextMarshaler and log/slog, so a credential never
// reaches a log or error message by accident. Fields stay readable by code.
type Credential interface {
	Kind() CredentialKind
	// Expiry is the time after which the credential must not be used; zero
	// means no known expiry.
	Expiry() time.Time
	sealed()
}

// Secret is an opaque secret for non-mail integrations.
type Secret struct {
	Bytes     []byte
	ExpiresAt time.Time
}

// Password is a user name and password. NTLM uses it when User is
// domain-qualified as "DOMAIN\user"; a "user@domain" name is an ordinary
// login name and never selects NTLM.
type Password struct {
	User   string
	Secret []byte
	// AuthzID is the optional authorization identity: the account to act as
	// when it differs from User.
	AuthzID string
}

// OAuth2 is a bearer access token for User. The host owns consent, refresh
// tokens and rotation; uma only ever sees the access token.
type OAuth2 struct {
	User        string
	AccessToken string
	ExpiresAt   time.Time
	// AuthzID is the optional mailbox or identity to act as, for example a
	// shared mailbox address.
	AuthzID string
}

// ClientCert is a TLS client certificate and its private key.
type ClientCert struct {
	// Certificate is presented during the TLS handshake. Expiry reports
	// Leaf.NotAfter when Leaf is set.
	Certificate tls.Certificate
	// AuthzID is the optional authorization identity sent with SASL EXTERNAL.
	AuthzID string
}

// Kerberos is a Kerberos principal and the material to obtain service
// tickets for it: a keytab, or a credential cache holding a ticket-granting
// ticket. Exactly one of Keytab and CCache is set. Both are the MIT file
// formats (what ktutil and kinit write), so a host produces them with
// standard tooling and uma core stays free of a Kerberos implementation.
type Kerberos struct {
	// Principal is the client principal name without the realm ("alice",
	// "svc/host").
	Principal string
	// Realm is the principal's realm ("EXAMPLE.COM").
	Realm string
	// Keytab is the content of a keytab file holding the principal's keys.
	Keytab []byte
	// CCache is the content of a credential cache file holding a
	// ticket-granting ticket for the principal.
	CCache []byte
	// ExpiresAt is when the credential stops working: the ticket-granting
	// ticket's end time for a CCache; zero for a Keytab (fresh tickets per
	// connection).
	ExpiresAt time.Time
	// AuthzID is the optional authorization identity: the account to act as
	// when it differs from Principal.
	AuthzID string
}

func (Secret) Kind() CredentialKind     { return KindSecret }
func (Password) Kind() CredentialKind   { return KindPassword }
func (OAuth2) Kind() CredentialKind     { return KindOAuth2 }
func (ClientCert) Kind() CredentialKind { return KindClientCert }
func (Kerberos) Kind() CredentialKind   { return KindKerberos }

func (s Secret) Expiry() time.Time   { return s.ExpiresAt }
func (Password) Expiry() time.Time   { return time.Time{} }
func (o OAuth2) Expiry() time.Time   { return o.ExpiresAt }
func (k Kerberos) Expiry() time.Time { return k.ExpiresAt }

func (c ClientCert) Expiry() time.Time {
	if c.Certificate.Leaf != nil {
		return c.Certificate.Leaf.NotAfter
	}
	return time.Time{}
}

func (Secret) sealed()     {}
func (Password) sealed()   {}
func (OAuth2) sealed()     {}
func (ClientCert) sealed() {}
func (Kerberos) sealed()   {}

// redacted is the only text any credential renders as.
const redacted = "[redacted]"

var redactedJSON = []byte(`"` + redacted + `"`)

func writeRedacted(f fmt.State) { _, _ = io.WriteString(f, redacted) }

func (Secret) Format(f fmt.State, _ rune)     { writeRedacted(f) }
func (Password) Format(f fmt.State, _ rune)   { writeRedacted(f) }
func (OAuth2) Format(f fmt.State, _ rune)     { writeRedacted(f) }
func (ClientCert) Format(f fmt.State, _ rune) { writeRedacted(f) }
func (Kerberos) Format(f fmt.State, _ rune)   { writeRedacted(f) }

func (Secret) String() string     { return redacted }
func (Password) String() string   { return redacted }
func (OAuth2) String() string     { return redacted }
func (ClientCert) String() string { return redacted }
func (Kerberos) String() string   { return redacted }

func (Secret) GoString() string     { return redacted }
func (Password) GoString() string   { return redacted }
func (OAuth2) GoString() string     { return redacted }
func (ClientCert) GoString() string { return redacted }
func (Kerberos) GoString() string   { return redacted }

func (Secret) LogValue() slog.Value     { return slog.StringValue(redacted) }
func (Password) LogValue() slog.Value   { return slog.StringValue(redacted) }
func (OAuth2) LogValue() slog.Value     { return slog.StringValue(redacted) }
func (ClientCert) LogValue() slog.Value { return slog.StringValue(redacted) }
func (Kerberos) LogValue() slog.Value   { return slog.StringValue(redacted) }

func (Secret) MarshalJSON() ([]byte, error)     { return slices.Clone(redactedJSON), nil }
func (Password) MarshalJSON() ([]byte, error)   { return slices.Clone(redactedJSON), nil }
func (OAuth2) MarshalJSON() ([]byte, error)     { return slices.Clone(redactedJSON), nil }
func (ClientCert) MarshalJSON() ([]byte, error) { return slices.Clone(redactedJSON), nil }
func (Kerberos) MarshalJSON() ([]byte, error)   { return slices.Clone(redactedJSON), nil }

func (Secret) MarshalText() ([]byte, error)     { return []byte(redacted), nil }
func (Password) MarshalText() ([]byte, error)   { return []byte(redacted), nil }
func (OAuth2) MarshalText() ([]byte, error)     { return []byte(redacted), nil }
func (ClientCert) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Kerberos) MarshalText() ([]byte, error)   { return []byte(redacted), nil }

// CredentialRequest asks the host for the credential behind one channel's
// CredentialRef, for one connection attempt.
type CredentialRequest struct {
	Channel model.ChannelRef
	// Reference is the channel's CredentialRef, opaque to uma.
	Reference string
	// Purpose is adapter-defined and declared in its descriptor, e.g. "imap",
	// "smtp.submission", "smtp.relay", "graph".
	Purpose string
	// Accept lists the kinds the adapter can use for Purpose, preferred
	// first. The host returns one of them or a model.Unsupported error.
	Accept []CredentialKind
	// Fresh reports that the previous credential was rejected by the
	// provider: the host bypasses any cache and returns a newly issued
	// credential (for OAuth2, a refreshed access token).
	Fresh bool
}

// Accepts reports whether k is listed in Accept.
func (r CredentialRequest) Accepts(k CredentialKind) bool {
	return slices.Contains(r.Accept, k)
}

// Credentials resolves a credential per connection attempt. The host owns
// consent, refresh, rotation and storage. Errors are *model.Error: consent
// missing or revoked is model.Unauthorized, a kind outside Accept is
// model.Unsupported, an unreachable store is model.Unavailable or
// model.Transient.
//
// Adapters call Resolve for every connection attempt and never keep a
// credential past its Expiry or across connections. Byte slices in a
// returned credential are read-only to the adapter; the host may zero them
// once the connection ends.
type Credentials interface {
	Resolve(context.Context, CredentialRequest) (Credential, error)
}
