package telegram

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
)

func verify(t *testing.T, c *credentials, header http.Header) error {
	t.Helper()
	v, err := newVerifier(model.Channel{Ref: testChannel, CredentialRef: "telegram"}, c, &fixedClock{testNow})
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	_, err = v.Verify(t.Context(), webhookauth.Request{Header: header, Body: []byte(`{"update_id":1}`)})
	return err
}

func withToken(v string) http.Header {
	h := http.Header{}
	h.Set(HeaderSecretToken, v)
	return h
}

func TestSecretTokenAccepted(t *testing.T) {
	c := newCredentials()
	if err := verify(t, c, withToken(testSecret)); err != nil {
		t.Fatalf("verify = %v, want accepted", err)
	}
	// Only the webhook purpose is resolved: the bot token never takes part
	// in checking a request.
	if got := c.purposes(); !slices.Equal(got, []string{CredentialPurposeWebhook}) {
		t.Errorf("credential purposes = %v, want only %s", got, CredentialPurposeWebhook)
	}
	if r := c.requests[0]; r.Channel != testChannel || r.Reference != "telegram" {
		t.Errorf("request names %v %q, want the channel and its reference", r.Channel, r.Reference)
	}
}

func TestSecretTokenRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		want   error
	}{
		{"none", http.Header{}, webhookauth.ErrMissing},
		{"wrong", withToken("webhook-secret_0123456789-abcdeX"), webhookauth.ErrInvalid},
		{"prefix of the secret", withToken(testSecret[:len(testSecret)-1]), webhookauth.ErrInvalid},
		{"longer than the secret", withToken(testSecret + "x"), webhookauth.ErrInvalid},
		{"bearer form", withToken("Bearer " + testSecret), webhookauth.ErrInvalid},
		{"empty", withToken(""), webhookauth.ErrMissing},
		{"repeated", http.Header{HeaderSecretToken: {testSecret, testSecret}}, webhookauth.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verify(t, newCredentials(), tc.header)
			if !errors.Is(err, tc.want) {
				t.Fatalf("verify = %v, want %v", err, tc.want)
			}
			if got := webhookauth.Status(err); got < 400 || got > 499 {
				t.Errorf("status %d, want a client error", got)
			}
		})
	}
}

// A secret shorter than MinSecretBytes is refused whatever the request
// carries: a request echoing it must not pass.
func TestShortSecretRefusesEverything(t *testing.T) {
	for _, secret := range []string{"", "short", testSecret[:MinSecretBytes-1]} {
		c := newCredentials()
		c.secret = secret
		sent := secret
		if sent == "" {
			sent = testSecret // an empty header counts as none
		}
		err := verify(t, c, withToken(sent))
		if !errors.Is(err, webhookauth.ErrSecret) || webhookauth.Status(err) != http.StatusServiceUnavailable {
			t.Errorf("secret %q: verify = %v (status %d), want ErrSecret and 503", secret, err, webhookauth.Status(err))
		}
	}
	c := newCredentials()
	c.secret = testSecret[:MinSecretBytes]
	if err := verify(t, c, withToken(c.secret)); err != nil {
		t.Errorf("a secret of exactly %d bytes: verify = %v, want accepted", MinSecretBytes, err)
	}
}

func TestSecretUnresolvableIsTheHostsFailure(t *testing.T) {
	c := newCredentials()
	c.err = errors.New("vault down")
	err := verify(t, c, withToken(testSecret))
	if err == nil || webhookauth.Status(err) < 500 {
		t.Fatalf("verify = %v (status %d), want a server error: the request is not at fault", err, webhookauth.Status(err))
	}
}
