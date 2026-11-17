package secret

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// envelopeVersion is the only envelope version this package decodes.
const envelopeVersion = 1

// decode parses one stored envelope. Error messages are fixed text that
// names at most a field this package defines; stored text never reaches
// them. Every intermediate copy of a field is cleared before returning.
func decode(data []byte) (ports.Credential, error) {
	var fields map[string]json.RawMessage
	err := json.Unmarshal(data, &fields)
	r := &reader{fields: fields, used: map[string]bool{}}
	defer r.clear()
	if err != nil || fields == nil {
		return nil, malformed("not a JSON object")
	}

	v, ok := r.raw("v")
	if !ok {
		return nil, malformed("field v missing")
	}
	var version int
	if json.Unmarshal(v, &version) != nil {
		return nil, malformed("field v is not an integer")
	}
	if version != envelopeVersion {
		return nil, model.NewError(model.Unsupported, "stored credential envelope version not supported")
	}

	kind, err := r.str("kind", true)
	if err != nil {
		return nil, err
	}
	var cred ports.Credential
	switch ports.CredentialKind(kind) {
	case ports.KindSecret:
		cred, err = r.secret()
	case ports.KindPassword:
		cred, err = r.password()
	case ports.KindOAuth2:
		cred, err = r.oauth2()
	case ports.KindClientCert:
		cred, err = r.clientCert()
	case ports.KindKerberos:
		cred, err = r.kerberos()
	default:
		return nil, malformed("unknown kind")
	}
	if err == nil {
		err = r.done()
	}
	if err != nil {
		if cred != nil {
			zeroCredential(cred)
		}
		return nil, err
	}
	return cred, nil
}

func malformed(reason string) *model.Error {
	return model.NewError(model.Permanent, "stored credential is malformed: "+reason)
}

// reader reads envelope fields and records which were read, so unknown
// fields are rejected.
type reader struct {
	fields map[string]json.RawMessage
	used   map[string]bool
}

func (r *reader) clear() {
	for _, raw := range r.fields {
		clear(raw)
	}
}

// done fails when the envelope holds a field the kind does not define. The
// field name is not echoed: it is stored text.
func (r *reader) done() error {
	for name := range r.fields {
		if !r.used[name] {
			return malformed("unknown field")
		}
	}
	return nil
}

func (r *reader) raw(name string) (json.RawMessage, bool) {
	r.used[name] = true
	raw, ok := r.fields[name]
	return raw, ok
}

// bytes returns a JSON string field as a new slice owned by the caller.
// An absent field returns nil; a present one must be a string.
func (r *reader) bytes(name string, required bool) ([]byte, error) {
	raw, ok := r.raw(name)
	if !ok {
		if required {
			return nil, malformed("field " + name + " missing")
		}
		return nil, nil
	}
	b, ok := unquote(raw)
	if !ok {
		return nil, malformed("field " + name + " is not a string")
	}
	if required && len(b) == 0 {
		return nil, malformed("field " + name + " is empty")
	}
	return b, nil
}

func (r *reader) str(name string, required bool) (string, error) {
	b, err := r.bytes(name, required)
	if err != nil {
		return "", err
	}
	s := string(b)
	clear(b)
	return s, nil
}

// base64 decodes a standard-encoding base64 string field.
func (r *reader) base64(name string) ([]byte, bool, error) {
	text, err := r.bytes(name, false)
	if err != nil || text == nil {
		return nil, false, err
	}
	defer clear(text)
	out := make([]byte, base64.StdEncoding.DecodedLen(len(text)))
	n, err := base64.StdEncoding.Decode(out, text)
	if err != nil {
		clear(out)
		return nil, true, malformed("field " + name + " is not valid base64")
	}
	if n == 0 {
		return nil, true, malformed("field " + name + " is empty")
	}
	return out[:n], true, nil
}

func (r *reader) time(name string) (time.Time, error) {
	if _, ok := r.fields[name]; !ok {
		r.used[name] = true
		return time.Time{}, nil
	}
	s, err := r.str(name, true)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, malformed("field " + name + " is not an RFC 3339 time")
	}
	return t, nil
}

// exactlyOne reads a secret given either as text (textField) or as base64
// (b64Field), never both.
func (r *reader) exactlyOne(textField, b64Field string) ([]byte, error) {
	text, err := r.bytes(textField, false)
	if err != nil {
		return nil, err
	}
	bin, present, err := r.base64(b64Field)
	if err != nil {
		clear(text)
		return nil, err
	}
	switch {
	case text != nil && present:
		clear(text)
		clear(bin)
		return nil, malformed("fields " + textField + " and " + b64Field + " are exclusive")
	case present:
		return bin, nil
	case text == nil:
		return nil, malformed("field " + textField + " or " + b64Field + " required")
	case len(text) == 0:
		return nil, malformed("field " + textField + " is empty")
	}
	return text, nil
}

func (r *reader) secret() (ports.Credential, error) {
	value, err := r.exactlyOne("value", "value_base64")
	if err != nil {
		return nil, err
	}
	exp, err := r.time("expires_at")
	if err != nil {
		clear(value)
		return nil, err
	}
	return ports.Secret{Bytes: value, ExpiresAt: exp}, nil
}

func (r *reader) password() (ports.Credential, error) {
	user, err := r.str("user", true)
	if err != nil {
		return nil, err
	}
	pass, err := r.bytes("password", true)
	if err != nil {
		return nil, err
	}
	authz, err := r.str("authzid", false)
	if err != nil {
		clear(pass)
		return nil, err
	}
	return ports.Password{User: user, Secret: pass, AuthzID: authz}, nil
}

func (r *reader) oauth2() (ports.Credential, error) {
	token, err := r.str("access_token", true)
	if err != nil {
		return nil, err
	}
	user, err := r.str("user", false)
	if err != nil {
		return nil, err
	}
	exp, err := r.time("expires_at")
	if err != nil {
		return nil, err
	}
	authz, err := r.str("authzid", false)
	if err != nil {
		return nil, err
	}
	return ports.OAuth2{User: user, AccessToken: token, ExpiresAt: exp, AuthzID: authz}, nil
}

func (r *reader) clientCert() (ports.Credential, error) {
	certPEM, err := r.bytes("certificate_pem", true)
	if err != nil {
		return nil, err
	}
	defer clear(certPEM)
	keyPEM, err := r.bytes("private_key_pem", true)
	if err != nil {
		return nil, err
	}
	defer clear(keyPEM)
	authz, err := r.str("authzid", false)
	if err != nil {
		return nil, err
	}
	// X509KeyPair sets Leaf, so Expiry reports the certificate's NotAfter.
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, malformed("certificate_pem and private_key_pem are not a valid key pair")
	}
	return ports.ClientCert{Certificate: cert, AuthzID: authz}, nil
}

func (r *reader) kerberos() (ports.Credential, error) {
	principal, err := r.str("principal", true)
	if err != nil {
		return nil, err
	}
	realm, err := r.str("realm", true)
	if err != nil {
		return nil, err
	}
	keytab, hasKeytab, err := r.base64("keytab_base64")
	if err != nil {
		return nil, err
	}
	ccache, hasCCache, err := r.base64("ccache_base64")
	if err != nil {
		clear(keytab)
		return nil, err
	}
	k := ports.Kerberos{Principal: principal, Realm: realm, Keytab: keytab, CCache: ccache}
	if hasKeytab == hasCCache {
		zeroCredential(k)
		return nil, malformed("exactly one of keytab_base64 and ccache_base64 required")
	}
	if k.ExpiresAt, err = r.time("expires_at"); err != nil {
		zeroCredential(k)
		return nil, err
	}
	if k.AuthzID, err = r.str("authzid", false); err != nil {
		zeroCredential(k)
		return nil, err
	}
	return k, nil
}

// unquote decodes a JSON string literal into a new slice without an
// intermediate Go string, so the result can be cleared. raw comes from a
// document json.Unmarshal already validated. It reports false when raw is
// not a string (null, number, object, ...). Escapes only shrink, so the
// output never outgrows its first allocation and leaves no copy behind.
// A lone surrogate decodes as U+FFFD, as in encoding/json.
func unquote(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	s := raw[1 : len(raw)-1]
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(s) {
			clear(out)
			return nil, false
		}
		e := s[i+1]
		i += 2
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := hex4(s[i:])
			if !ok {
				clear(out)
				return nil, false
			}
			i += 4
			if utf16.IsSurrogate(r) {
				dec := utf8.RuneError
				if i+6 <= len(s) && s[i] == '\\' && s[i+1] == 'u' {
					if r2, ok := hex4(s[i+2:]); ok {
						if dec = utf16.DecodeRune(r, r2); dec != utf8.RuneError {
							i += 6
						}
					}
				}
				r = dec
			}
			out = utf8.AppendRune(out, r)
		default:
			clear(out)
			return nil, false
		}
	}
	return out, true
}

// hex4 parses four hex digits at the start of b.
func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(c)
	}
	return r, true
}
