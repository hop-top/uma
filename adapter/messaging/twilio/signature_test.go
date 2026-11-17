package twilio_test

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Twilio's documented webhook signature algorithm
	"encoding/base64"
	"errors"
	"net/url"
	"sort"
	"testing"

	"hop.top/uma/adapter/messaging/twilio"
)

const formType = "application/x-www-form-urlencoded"

// sign computes X-Twilio-Signature independently of the code under test:
// HMAC-SHA1 over the URL followed by each sorted parameter name and value.
func sign(token, rawURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := rawURL
	for _, k := range keys {
		s += k + form.Get(k)
	}
	m := hmac.New(sha1.New, []byte(token))
	m.Write([]byte(s))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// The worked example of https://www.twilio.com/docs/usage/security.
func TestVerifyFormDocumentedExample(t *testing.T) {
	const u = "https://example.com/myapp.php?foo=1&bar=2"
	form := url.Values{
		"CallSid": {"CA1234567890ABCDE"}, "Caller": {"+14158675310"}, "Digits": {"1234"},
		"From": {"+14158675310"}, "To": {"+18005551212"},
	}
	const want = "L/OH5YylLD5NRKLltdqwSvS0BnU="
	if got := sign("12345", u, form); got != want {
		t.Fatalf("reference signer = %s, want the documented %s", got, want)
	}
	got, err := twilio.VerifyForm([]byte("12345"), u, formType, []byte(form.Encode()), want)
	if err != nil {
		t.Fatalf("VerifyForm(documented example) = %v", err)
	}
	if got.Get("Digits") != "1234" {
		t.Fatalf("form = %v", got)
	}
}

func TestVerifyFormRejects(t *testing.T) {
	const u = "https://hooks.example.com/twilio/sms"
	form := url.Values{"MessageSid": {"SM00000000000000000000000000000001"}, "Body": {"hi"}, "From": {"+15005550001"}}
	body := []byte(form.Encode())
	good := sign("token", u, form)
	if _, err := twilio.VerifyForm([]byte("token"), u, formType+"; charset=utf-8", body, good); err != nil {
		t.Fatalf("valid request = %v", err)
	}
	tampered := url.Values{"MessageSid": form["MessageSid"], "Body": {"bye"}, "From": form["From"]}
	cases := []struct {
		name, token, url, ctype string
		body                    []byte
		sig                     string
		want                    error
	}{
		{"missing signature", "token", u, formType, body, "", twilio.ErrSignature},
		{"wrong token", "other", u, formType, body, good, twilio.ErrSignature},
		{"no token", "", u, formType, body, good, twilio.ErrSignature},
		{"other url", "token", u + "?x=1", formType, body, good, twilio.ErrSignature},
		{"tampered body", "token", u, formType, []byte(tampered.Encode()), good, twilio.ErrSignature},
		{"repeated parameter", "token", u, formType, append(body, []byte("&Body=evil")...), good, twilio.ErrMalformed},
		{"json body", "token", u, "application/json", body, good, twilio.ErrMalformed},
		{"bad encoding", "token", u, formType, []byte("Body=%zz"), good, twilio.ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := twilio.VerifyForm([]byte(c.token), c.url, c.ctype, c.body, c.sig)
			if !errors.Is(err, c.want) || got != nil {
				t.Fatalf("VerifyForm = %v, %v; want %v and no form", got, err, c.want)
			}
		})
	}
}
