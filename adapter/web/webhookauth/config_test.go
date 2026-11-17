package webhookauth_test

import (
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
)

func TestNewRefusesUnsafeConfig(t *testing.T) {
	src := secrets(longSecret)
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
		src  webhookauth.SecretSource
	}{
		{"zero config: no auth is never a default", webhookauth.Config{}, src},
		{"unknown scheme", webhookauth.Config{Scheme: "none"}, src},
		{"scheme from the wire is not a thing", webhookauth.Config{Scheme: "hmac-sha256"}, src},
		{"no secret source", webhookauth.Config{Scheme: webhookauth.SchemeBearer}, nil},
		{"header with a space", webhookauth.Config{Scheme: webhookauth.SchemeToken, Header: "X Token"}, src},
		{"header with a colon", webhookauth.Config{Scheme: webhookauth.SchemeToken, Header: "X-Token:"}, src},
		{"header with a newline", webhookauth.Config{Scheme: webhookauth.SchemeToken, Header: "X-Token\nX-Other"}, src},
		{"negative minimum", webhookauth.Config{Scheme: webhookauth.SchemeToken, MinSecretBytes: -1}, src},
		{"negative body limit", webhookauth.Config{Scheme: webhookauth.SchemeToken, MaxBodyBytes: -1}, src},
		{"encoded token key", webhookauth.Config{Scheme: webhookauth.SchemeToken, KeyEncoding: "hex"}, src},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhookauth.New(tc.cfg, tc.src, webhookauth.Deps{})
			wantErr(t, err, webhookauth.ErrConfig)
		})
	}
}
