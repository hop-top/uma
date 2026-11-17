// Package secret provides a [ports.Credentials] backed by a hop.top/kit
// secret store ([hop.top/kit/go/storage/secret.Store]: Get, List, Exists).
//
// The host picks the backend (keyring, file, OpenBao, 1Password, its own
// implementation), configures and constructs it, and passes it to [New].
// This package imports kit's interfaces only, never a backend: it
// registers nothing, reads no environment variable and starts nothing.
//
// # Keys
//
// Each [ports.CredentialRequest] maps to one secret key. [DefaultKey] uses
//
//	uma/<tenant>/<reference>
//
// (path-escaped segments; Purpose ignored). [WithKeyFunc] replaces it, for
// example to key by Purpose or to deny references a tenant may not use.
//
// # Stored value
//
// The secret's value is a JSON object, the envelope. "v" is the envelope
// version (1); "kind" is a [ports.CredentialKind] and selects the other
// fields. Unknown fields, a missing required field or a field of the wrong
// type fail with model.Permanent; another version fails with
// model.Unsupported. Times are RFC 3339 strings. Text fields are JSON
// strings; binary fields are standard base64 with padding.
//
//	kind         field            required  maps to
//	secret       value            one of    Secret.Bytes (UTF-8 text)
//	             value_base64     one of    Secret.Bytes (binary)
//	             expires_at       no        Secret.ExpiresAt
//	password     user             yes       Password.User
//	             password         yes       Password.Secret
//	             authzid          no        Password.AuthzID
//	oauth2       access_token     yes       OAuth2.AccessToken
//	             user             no        OAuth2.User
//	             expires_at       no        OAuth2.ExpiresAt
//	             authzid          no        OAuth2.AuthzID
//	client_cert  certificate_pem  yes       ClientCert.Certificate (PEM chain, leaf first)
//	             private_key_pem  yes       ClientCert.Certificate.PrivateKey (PEM)
//	             authzid          no        ClientCert.AuthzID
//	kerberos     principal        yes       Kerberos.Principal (no realm)
//	             realm            yes       Kerberos.Realm
//	             keytab_base64    one of    Kerberos.Keytab (MIT keytab file)
//	             ccache_base64    one of    Kerberos.CCache (MIT credential cache file)
//	             expires_at       no        Kerberos.ExpiresAt
//	             authzid          no        Kerberos.AuthzID
//
// For example:
//
//	{"v":1,"kind":"password","user":"support@example.com","password":"app-password"}
//	{"v":1,"kind":"oauth2","user":"support@example.com","access_token":"ya29...","expires_at":"2026-10-11T12:00:00Z"}
//
// # Resolve
//
// Resolve reads the store on every call. Nothing is cached, so a value the
// host rotated is used on the next attempt and Fresh needs no handling
// here: whatever refreshes OAuth2 tokens writes the new token to the store.
//
// A stored kind outside [ports.CredentialRequest.Accept] fails with
// model.Unsupported; an empty Accept with model.Validation. A missing key
// ([hop.top/kit/go/storage/secret.ErrNotFound]) is model.NotFound, a store that cannot read
// ([hop.top/kit/go/storage/secret.ErrNotSupported]) model.Unsupported and any other store
// failure model.Unavailable. Context cancellation and deadline errors are
// returned as the standard context errors.
//
// # Secret material
//
// Errors carry fixed text only. Store errors are dropped, never wrapped,
// because backends may echo keys or values; envelope errors name at most
// one of the field names above, never stored text.
//
// The store's value is cleared after decoding, so the store must hand
// Resolve a slice it does not reuse (every kit backend returns a fresh one
// per Get). Decoding copies secret text straight into the returned
// credential's byte slices, with no intermediate string, and clears every
// intermediate buffer. What cannot be cleared: the OAuth2 access token and
// the names (ports declares them as strings) and the parsed private key of
// a client certificate.
package secret
