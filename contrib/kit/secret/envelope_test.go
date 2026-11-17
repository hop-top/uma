package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func resolveStored(t *testing.T, value []byte, accept ...ports.CredentialKind) (ports.Credential, error) {
	t.Helper()
	if len(accept) == 0 {
		accept = allKinds
	}
	store := newStore()
	store.put(defaultKey, value)
	return mustNew(t, store).Resolve(context.Background(), request(accept...))
}

func TestDecodeEachKind(t *testing.T) {
	exp := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
	keytab := []byte{0x05, 0x02, 0x00, 0xff, 0x10}
	ccache := []byte{0x05, 0x04, 0x00, 0x0c, 0x01}
	binary := []byte{0x00, 0xfe, 0x7f, 0x80}

	tests := []struct {
		name  string
		value []byte
		check func(t *testing.T, c ports.Credential)
	}{
		{
			name:  "secret text",
			value: envelope(t, map[string]any{"v": 1, "kind": "secret", "value": "api-key-1", "expires_at": exp.Format(time.RFC3339)}),
			check: func(t *testing.T, c ports.Credential) {
				s := c.(ports.Secret)
				if string(s.Bytes) != "api-key-1" || !s.ExpiresAt.Equal(exp) {
					t.Fatalf("secret = %q, %v", s.Bytes, s.ExpiresAt)
				}
			},
		},
		{
			name:  "secret base64",
			value: envelope(t, map[string]any{"v": 1, "kind": "secret", "value_base64": base64.StdEncoding.EncodeToString(binary)}),
			check: func(t *testing.T, c ports.Credential) {
				s := c.(ports.Secret)
				if !bytes.Equal(s.Bytes, binary) || !s.ExpiresAt.IsZero() {
					t.Fatalf("secret = %x, %v", s.Bytes, s.ExpiresAt)
				}
			},
		},
		{
			name: "password with escapes",
			// ä, an escaped quote and backslash, a newline and a
			// surrogate pair.
			value: []byte(`{"v":1,"kind":"password","user":"DOMAIN\\alice","password":"päss\"\\\n😀\/","authzid":"shared@example.com"}`),
			check: func(t *testing.T, c ports.Credential) {
				p := c.(ports.Password)
				if p.User != `DOMAIN\alice` || string(p.Secret) != "päss\"\\\n😀/" || p.AuthzID != "shared@example.com" {
					t.Fatalf("password = %q, %q, %q", p.User, p.Secret, p.AuthzID)
				}
			},
		},
		{
			name:  "oauth2",
			value: envelope(t, map[string]any{"v": 1, "kind": "oauth2", "user": "alice@example.com", "access_token": "ya29.token", "expires_at": exp.Format(time.RFC3339Nano), "authzid": "box@example.com"}),
			check: func(t *testing.T, c ports.Credential) {
				o := c.(ports.OAuth2)
				if o.User != "alice@example.com" || o.AccessToken != "ya29.token" || !o.ExpiresAt.Equal(exp) || o.AuthzID != "box@example.com" {
					t.Fatalf("oauth2 = %q, %q, %v, %q", o.User, o.AccessToken, o.ExpiresAt, o.AuthzID)
				}
			},
		},
		{
			name:  "kerberos keytab",
			value: envelope(t, map[string]any{"v": 1, "kind": "kerberos", "principal": "svc/host", "realm": "EXAMPLE.COM", "keytab_base64": base64.StdEncoding.EncodeToString(keytab), "authzid": "alice"}),
			check: func(t *testing.T, c ports.Credential) {
				k := c.(ports.Kerberos)
				if k.Principal != "svc/host" || k.Realm != "EXAMPLE.COM" || !bytes.Equal(k.Keytab, keytab) || k.CCache != nil || k.AuthzID != "alice" {
					t.Fatalf("kerberos = %+v", k)
				}
			},
		},
		{
			name:  "kerberos ccache",
			value: envelope(t, map[string]any{"v": 1, "kind": "kerberos", "principal": "alice", "realm": "EXAMPLE.COM", "ccache_base64": base64.StdEncoding.EncodeToString(ccache), "expires_at": exp.Format(time.RFC3339)}),
			check: func(t *testing.T, c ports.Credential) {
				k := c.(ports.Kerberos)
				if !bytes.Equal(k.CCache, ccache) || k.Keytab != nil || !k.ExpiresAt.Equal(exp) {
					t.Fatalf("kerberos = %+v", k)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := resolveStored(t, tt.value)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestDecodeClientCert(t *testing.T) {
	notAfter := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	certPEM, keyPEM := selfSigned(t, notAfter)
	c, err := resolveStored(t, envelope(t, map[string]any{
		"v": 1, "kind": "client_cert", "certificate_pem": string(certPEM), "private_key_pem": string(keyPEM), "authzid": "alice",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cc := c.(ports.ClientCert)
	if len(cc.Certificate.Certificate) != 1 || cc.Certificate.PrivateKey == nil || cc.AuthzID != "alice" {
		t.Fatalf("client cert = %d certs, key %T, authzid %q", len(cc.Certificate.Certificate), cc.Certificate.PrivateKey, cc.AuthzID)
	}
	if !cc.Expiry().Equal(notAfter) {
		t.Fatalf("expiry = %v, want %v", cc.Expiry(), notAfter)
	}
}

// TestDecodeKindMatchesEnvelope pins kind dispatch: each envelope kind
// yields exactly that credential kind and concrete type.
func TestDecodeKindMatchesEnvelope(t *testing.T) {
	certPEM, keyPEM := selfSigned(t, time.Now().Add(time.Hour))
	values := map[ports.CredentialKind][]byte{
		ports.KindSecret:     envelope(t, map[string]any{"v": 1, "kind": "secret", "value": "s"}),
		ports.KindPassword:   envelope(t, map[string]any{"v": 1, "kind": "password", "user": "u", "password": "p"}),
		ports.KindOAuth2:     envelope(t, map[string]any{"v": 1, "kind": "oauth2", "access_token": "t"}),
		ports.KindClientCert: envelope(t, map[string]any{"v": 1, "kind": "client_cert", "certificate_pem": string(certPEM), "private_key_pem": string(keyPEM)}),
		ports.KindKerberos:   envelope(t, map[string]any{"v": 1, "kind": "kerberos", "principal": "p", "realm": "R", "keytab_base64": "AQ=="}),
	}
	for kind, v := range values {
		c, err := resolveStored(t, v, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if c.Kind() != kind {
			t.Fatalf("envelope kind %s decoded as %s", kind, c.Kind())
		}
		var ok bool
		switch kind {
		case ports.KindSecret:
			_, ok = c.(ports.Secret)
		case ports.KindPassword:
			_, ok = c.(ports.Password)
		case ports.KindOAuth2:
			_, ok = c.(ports.OAuth2)
		case ports.KindClientCert:
			_, ok = c.(ports.ClientCert)
		case ports.KindKerberos:
			_, ok = c.(ports.Kerberos)
		}
		if !ok {
			t.Fatalf("envelope kind %s decoded as %T", kind, c)
		}
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	certPEM, keyPEM := selfSigned(t, time.Now().Add(time.Hour))
	otherCert, _ := selfSigned(t, time.Now().Add(time.Hour))
	tests := []struct {
		name  string
		value string
		code  model.ErrorCode
	}{
		{"not json", `not json`, model.Permanent},
		{"empty", ``, model.Permanent},
		{"null", `null`, model.Permanent},
		{"array", `[1]`, model.Permanent},
		{"missing version", `{"kind":"secret","value":"s"}`, model.Permanent},
		{"version string", `{"v":"1","kind":"secret","value":"s"}`, model.Permanent},
		{"version float", `{"v":1.5,"kind":"secret","value":"s"}`, model.Permanent},
		{"future version", `{"v":2,"kind":"secret","value":"s"}`, model.Unsupported},
		{"missing kind", `{"v":1,"value":"s"}`, model.Permanent},
		{"unknown kind", `{"v":1,"kind":"ssh_key","value":"s"}`, model.Permanent},
		{"kind not string", `{"v":1,"kind":7}`, model.Permanent},
		{"unknown field", `{"v":1,"kind":"secret","value":"s","extra":"x"}`, model.Permanent},
		{"field of another kind", `{"v":1,"kind":"password","user":"u","password":"p","access_token":"t"}`, model.Permanent},
		{"secret both forms", `{"v":1,"kind":"secret","value":"s","value_base64":"cw=="}`, model.Permanent},
		{"secret neither form", `{"v":1,"kind":"secret"}`, model.Permanent},
		{"secret empty", `{"v":1,"kind":"secret","value":""}`, model.Permanent},
		{"secret bad base64", `{"v":1,"kind":"secret","value_base64":"!!"}`, model.Permanent},
		{"secret null", `{"v":1,"kind":"secret","value":null}`, model.Permanent},
		{"secret number", `{"v":1,"kind":"secret","value":12}`, model.Permanent},
		{"bad expiry", `{"v":1,"kind":"secret","value":"s","expires_at":"tomorrow"}`, model.Permanent},
		{"password missing user", `{"v":1,"kind":"password","password":"p"}`, model.Permanent},
		{"password missing password", `{"v":1,"kind":"password","user":"u"}`, model.Permanent},
		{"password expiry", `{"v":1,"kind":"password","user":"u","password":"p","expires_at":"2026-01-01T00:00:00Z"}`, model.Permanent},
		{"oauth2 missing token", `{"v":1,"kind":"oauth2","user":"u"}`, model.Permanent},
		{"client cert bad pem", `{"v":1,"kind":"client_cert","certificate_pem":"x","private_key_pem":"y"}`, model.Permanent},
		{"client cert missing key", `{"v":1,"kind":"client_cert","certificate_pem":` + quote(certPEM) + `}`, model.Permanent},
		{"client cert key mismatch", `{"v":1,"kind":"client_cert","certificate_pem":` + quote(otherCert) + `,"private_key_pem":` + quote(keyPEM) + `}`, model.Permanent},
		{"kerberos both", `{"v":1,"kind":"kerberos","principal":"p","realm":"R","keytab_base64":"AQ==","ccache_base64":"AQ=="}`, model.Permanent},
		{"kerberos neither", `{"v":1,"kind":"kerberos","principal":"p","realm":"R"}`, model.Permanent},
		{"kerberos missing realm", `{"v":1,"kind":"kerberos","principal":"p","keytab_base64":"AQ=="}`, model.Permanent},
		{"kerberos empty keytab", `{"v":1,"kind":"kerberos","principal":"p","realm":"R","keytab_base64":""}`, model.Permanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := resolveStored(t, []byte(tt.value))
			if err == nil {
				t.Fatalf("decoded %s, want error", c.Kind())
			}
			wantCode(t, err, tt.code)
		})
	}
}

func quote(b []byte) string {
	var buf bytes.Buffer
	buf.WriteByte('"')
	for _, c := range b {
		switch c {
		case '\n':
			buf.WriteString(`\n`)
		case '"', '\\':
			buf.WriteByte('\\')
			buf.WriteByte(c)
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
	return buf.String()
}
