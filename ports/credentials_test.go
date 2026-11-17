package ports_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"hop.top/uma/ports"
)

const redacted = "[redacted]"

// secretMarkers are the secret values planted in the fixtures; none may
// appear in any rendering.
var secretMarkers = []string{"s3cr3t", "hunter2", "tok-abc", "alice", "bob@example.com", "PRIVKEY", "KEYTAB", "CCACHE", "EXAMPLE.TEST"}

func credentials() []ports.Credential {
	exp := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	return []ports.Credential{
		ports.Secret{Bytes: []byte("s3cr3t"), ExpiresAt: exp},
		ports.Password{User: "alice", Secret: []byte("hunter2"), AuthzID: "bob@example.com"},
		ports.OAuth2{User: "alice", AccessToken: "tok-abc", ExpiresAt: exp, AuthzID: "bob@example.com"},
		ports.ClientCert{
			Certificate: tls.Certificate{Certificate: [][]byte{[]byte("PRIVKEY")}, PrivateKey: "PRIVKEY"},
			AuthzID:     "alice",
		},
		ports.Kerberos{
			Principal: "alice", Realm: "EXAMPLE.TEST", Keytab: []byte("KEYTAB"), CCache: []byte("CCACHE"),
			ExpiresAt: exp, AuthzID: "bob@example.com",
		},
	}
}

func assertRedacted(t *testing.T, what, got string) {
	t.Helper()
	for _, m := range secretMarkers {
		if strings.Contains(got, m) {
			t.Errorf("%s leaks %q: %s", what, m, got)
		}
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("%s = %q, want it to contain %q", what, got, redacted)
	}
}

func TestCredentialRedactionFmt(t *testing.T) {
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%10v", "%T=%v"}
	for _, c := range credentials() {
		t.Run(string(c.Kind()), func(t *testing.T) {
			for _, verb := range verbs {
				if verb == "%T=%v" {
					assertRedacted(t, verb, fmt.Sprintf(verb, c, c))
					continue
				}
				if got := fmt.Sprintf(verb, c); got != redacted {
					t.Errorf("Sprintf(%s) = %q, want %q", verb, got, redacted)
				}
			}
			// Pointer, nested exported field, slice, map value and error
			// wrapping all route through the same methods.
			nested := struct {
				Name string
				Cred ports.Credential
			}{"x", c}
			for verb, v := range map[string]any{
				"ptr %v":    ptrTo(c),
				"nested %v": nested, "nested %+v": nested, "nested %#v": nested,
				"slice %v": []ports.Credential{c}, "map %v": map[string]ports.Credential{"k": c},
				"error": fmt.Errorf("resolve: %v", c),
			} {
				var got string
				if err, ok := v.(error); ok {
					got = err.Error()
				} else {
					got = fmt.Sprintf(verb[strings.LastIndex(verb, " ")+1:], v)
				}
				assertRedacted(t, verb, got)
			}
			if got := fmt.Sprint(c); got != redacted {
				t.Errorf("Sprint = %q", got)
			}
			if got := c.(fmt.Stringer).String(); got != redacted {
				t.Errorf("String = %q", got)
			}
			if got := c.(fmt.GoStringer).GoString(); got != redacted {
				t.Errorf("GoString = %q", got)
			}
		})
	}
}

func ptrTo(c ports.Credential) any {
	switch v := c.(type) {
	case ports.Secret:
		return &v
	case ports.Password:
		return &v
	case ports.OAuth2:
		return &v
	case ports.ClientCert:
		return &v
	case ports.Kerberos:
		return &v
	}
	panic("unknown credential")
}

func TestCredentialRedactionEncoders(t *testing.T) {
	for _, c := range credentials() {
		t.Run(string(c.Kind()), func(t *testing.T) {
			b, err := json.Marshal(c)
			if err != nil || string(b) != `"`+redacted+`"` {
				t.Errorf("json.Marshal = %s, %v", b, err)
			}
			b, err = json.Marshal(map[string]any{"cred": c, "ptr": ptrTo(c)})
			if err != nil {
				t.Fatal(err)
			}
			assertRedacted(t, "json nested", string(b))
			b, err = xml.Marshal(struct {
				XMLName xml.Name `xml:"x"`
				Cred    ports.Credential
			}{Cred: c})
			if err != nil {
				t.Fatal(err)
			}
			assertRedacted(t, "xml", string(b))

			var text, js bytes.Buffer
			slog.New(slog.NewTextHandler(&text, nil)).Info("m", "cred", c, "ptr", ptrTo(c),
				slog.Group("g", "cred", c))
			slog.New(slog.NewJSONHandler(&js, nil)).Info("m", "cred", c, "ptr", ptrTo(c),
				slog.Group("g", "cred", c))
			for name, out := range map[string]string{"slog text": text.String(), "slog json": js.String()} {
				assertRedacted(t, name, out)
			}
			if !strings.Contains(text.String(), "cred="+redacted) || !strings.Contains(text.String(), "g.cred="+redacted) {
				t.Errorf("slog text = %s", text.String())
			}
			if !strings.Contains(js.String(), `"cred":"`+redacted+`"`) {
				t.Errorf("slog json = %s", js.String())
			}
			// Handlers prefer MarshalText/JSON when present, so check the
			// LogValuer directly: custom handlers may call only it.
			if lv, ok := c.(slog.LogValuer); !ok || lv.LogValue().String() != redacted {
				t.Errorf("LogValue missing or not redacted")
			}
		})
	}
}

func TestCredentialKindAndExpiry(t *testing.T) {
	exp := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	leaf := &x509.Certificate{NotAfter: exp}
	cases := []struct {
		cred   ports.Credential
		kind   ports.CredentialKind
		expiry time.Time
	}{
		{ports.Secret{ExpiresAt: exp}, ports.KindSecret, exp},
		{ports.Secret{}, ports.KindSecret, time.Time{}},
		{ports.Password{User: "u"}, ports.KindPassword, time.Time{}},
		{ports.OAuth2{ExpiresAt: exp}, ports.KindOAuth2, exp},
		{ports.OAuth2{}, ports.KindOAuth2, time.Time{}},
		{ports.ClientCert{Certificate: tls.Certificate{Leaf: leaf}}, ports.KindClientCert, exp},
		{ports.ClientCert{}, ports.KindClientCert, time.Time{}},
		{ports.Kerberos{ExpiresAt: exp}, ports.KindKerberos, exp},
		{ports.Kerberos{Keytab: []byte("k")}, ports.KindKerberos, time.Time{}},
	}
	for _, tc := range cases {
		if got := tc.cred.Kind(); got != tc.kind {
			t.Errorf("%T Kind = %q, want %q", tc.cred, got, tc.kind)
		}
		if got := tc.cred.Expiry(); !got.Equal(tc.expiry) {
			t.Errorf("%T Expiry = %v, want %v", tc.cred, got, tc.expiry)
		}
	}
	wire := map[ports.CredentialKind]string{
		ports.KindSecret: "secret", ports.KindPassword: "password",
		ports.KindOAuth2: "oauth2", ports.KindClientCert: "client_cert",
		ports.KindKerberos: "kerberos",
	}
	for k, want := range wire {
		if string(k) != want {
			t.Errorf("kind %q, want wire value %q", k, want)
		}
	}
}

func TestCredentialRequestAccepts(t *testing.T) {
	r := ports.CredentialRequest{Accept: []ports.CredentialKind{ports.KindOAuth2, ports.KindPassword}}
	if !r.Accepts(ports.KindOAuth2) || !r.Accepts(ports.KindPassword) {
		t.Error("listed kinds not accepted")
	}
	if r.Accepts(ports.KindClientCert) || (ports.CredentialRequest{}).Accepts(ports.KindSecret) {
		t.Error("unlisted kind accepted")
	}
}

// TestCredentialFieldsReadable pins that redaction is presentation only: code
// reads the values unchanged.
func TestCredentialFieldsReadable(t *testing.T) {
	var c ports.Credential = ports.Password{User: "alice", Secret: []byte("hunter2")}
	p, ok := c.(ports.Password)
	if !ok || p.User != "alice" || string(p.Secret) != "hunter2" {
		t.Fatalf("fields = %q %q", p.User, p.Secret)
	}
}
