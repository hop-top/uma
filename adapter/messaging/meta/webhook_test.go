package meta_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/meta"
)

// Meta signs the raw, escaped payload bytes: HMAC-SHA256 under the app
// secret, hex, "sha256=" prefix.
func TestSignMatchesDocumentedScheme(t *testing.T) {
	secret, body := []byte("app-secret"), []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if got := meta.Sign(secret, body); got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
}

func TestValidSignature(t *testing.T) {
	secret := []byte("app-secret")
	body := []byte(`{"text":"caf` + "\\" + `u00e9"}`)
	good := meta.Sign(secret, body)
	cases := map[string]struct {
		header string
		body   []byte
		secret []byte
		want   bool
	}{
		"valid":                {good, body, secret, true},
		"empty header":         {"", body, secret, false},
		"no prefix":            {strings.TrimPrefix(good, "sha256="), body, secret, false},
		"sha1 prefix":          {"sha1=" + strings.TrimPrefix(good, "sha256="), body, secret, false},
		"uppercase hex":        {"sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256=")), body, secret, true},
		"truncated":            {good[:len(good)-2], body, secret, false},
		"not hex":              {"sha256=" + strings.Repeat("zz", 32), body, secret, false},
		"other secret":         {meta.Sign([]byte("other"), body), body, secret, false},
		"empty secret":         {meta.Sign(nil, body), body, nil, false},
		"re-encoded body":      {good, []byte(`{"text":"café"}`), secret, false},
		"trailing byte":        {good, append(append([]byte(nil), body...), '\n'), secret, false},
		"signature for prefix": {meta.Sign(secret, body[:5]), body, secret, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := meta.ValidSignature(c.header, c.body, c.secret); got != c.want {
				t.Fatalf("ValidSignature = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVerifyChallenge(t *testing.T) {
	token := []byte("verify-me")
	q := func(mode, tok, challenge string) url.Values {
		v := url.Values{}
		if mode != "" {
			v.Set("hub.mode", mode)
		}
		if tok != "" {
			v.Set("hub.verify_token", tok)
		}
		if challenge != "" {
			v.Set("hub.challenge", challenge)
		}
		return v
	}
	if got, ok := meta.VerifyChallenge(q("subscribe", "verify-me", "1158201444"), token); !ok || got != "1158201444" {
		t.Fatalf("VerifyChallenge = %q, %v; want the challenge echoed", got, ok)
	}
	for name, v := range map[string]url.Values{
		"wrong token":    q("subscribe", "verify-you", "1"),
		"missing token":  q("subscribe", "", "1"),
		"wrong mode":     q("unsubscribe", "verify-me", "1"),
		"missing mode":   q("", "verify-me", "1"),
		"no challenge":   q("subscribe", "verify-me", ""),
		"token prefix":   q("subscribe", "verify", "1"),
		"token extended": q("subscribe", "verify-me-too", "1"),
		"script":         q("subscribe", "verify-me", "<script>alert(1)</script>"),
		"not an integer": q("subscribe", "verify-me", "12a"),
	} {
		if _, ok := meta.VerifyChallenge(v, token); ok {
			t.Errorf("%s: verified, want refused", name)
		}
	}
	if _, ok := meta.VerifyChallenge(q("subscribe", "", "1"), nil); ok {
		t.Error("empty configured token verified an empty presented token")
	}
}
