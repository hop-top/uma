package twilio_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/twilio"
)

const jsonType = "application/json"

// signURL computes X-Twilio-Signature of a JSON-bodied webhook as Twilio
// documents it (https://www.twilio.com/docs/usage/webhooks/webhooks-security):
// HMAC-SHA1 of the full URL, bodySHA256 query parameter included, with no
// POST parameters, since the body is not a form. It reuses the independent
// signer of signature_test.go.
func signURL(token, rawURL string) string {
	return sign(token, rawURL, nil)
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// The JSON example of https://www.twilio.com/docs/usage/webhooks/webhooks-security
// gives its body, the SHA-256 of it and a signature, but not the auth
// token that made the signature, so only the hash can be checked here.
func TestDocumentedJSONExampleHash(t *testing.T) {
	const body = `{"CallSid":"CA1234567890ABCDE","Caller":"+12349013030"}`
	const want = "5ccde7145dfb8f56479710896586cb9d5911809d83afbe34627818790db0aec9"
	if got := bodyHash([]byte(body)); got != want {
		t.Fatalf("bodySHA256 = %s, want the documented %s", got, want)
	}
}

// Twilio's own SDK test vector (twilio-go client/request_validator_test.go,
// token "12345"): a JSON body whose URL carries bodySHA256 after other
// query parameters. The signature is a constant of the SDK's suite, not
// computed by the code under test.
func TestVerifyJSONTwilioSDKVector(t *testing.T) {
	const (
		u    = "https://mycompany.com/myapp.php?foo=1&bar=2&bodySHA256=0a1ff7634d9ab3b95db5c9a2dfe9416e41502b283a80c7cf19632632f96e6620"
		sig  = "a9nBmqA0ju/hNViExpshrM61xv4="
		body = `{"property": "value", "boolean": true}`
	)
	if got := bodyHash([]byte(body)); got != "0a1ff7634d9ab3b95db5c9a2dfe9416e41502b283a80c7cf19632632f96e6620" {
		t.Fatalf("vector body hash = %s", got)
	}
	if got := signURL("12345", u); got != sig {
		t.Fatalf("reference signer = %s, want the SDK vector %s", got, sig)
	}
	got, err := twilio.VerifyJSON([]byte("12345"), u, jsonType, []byte(body), sig)
	if err != nil {
		t.Fatalf("VerifyJSON(SDK vector) = %v", err)
	}
	if got.Get("property") != "value" || got.Get("boolean") != "true" {
		t.Fatalf("fields = %v", got)
	}
	// The same through the dispatcher, with a charset.
	if _, err := twilio.Verify([]byte("12345"), u, jsonType+"; charset=utf-8", []byte(body), sig); err != nil {
		t.Fatalf("Verify(SDK vector) = %v", err)
	}
}

// A verified JSON object reads as the form of the same fields: scalars
// become their text, everything else is left out.
func TestVerifyJSONFields(t *testing.T) {
	const base = "https://hooks.example.com/twilio/sms"
	body := []byte(`{"MessageSid":"SM1","From":"+15005550001","Body":"héllo","NumMedia":2,"Flag":false,"Nothing":null,"Nested":{"a":"b"},"List":["x"],"Big":12345678901234567890}`)
	u := base + "?bodySHA256=" + bodyHash(body)
	got, err := twilio.VerifyJSON([]byte("token"), u, jsonType, body, signURL("token", u))
	if err != nil {
		t.Fatalf("VerifyJSON = %v", err)
	}
	want := url.Values{"MessageSid": {"SM1"}, "From": {"+15005550001"}, "Body": {"héllo"}, "NumMedia": {"2"}, "Flag": {"false"}, "Big": {"12345678901234567890"}}
	if len(got) != len(want) {
		t.Fatalf("fields = %v, want scalars only: %v", got, want)
	}
	for k := range want {
		if got.Get(k) != want.Get(k) {
			t.Fatalf("field %s = %q, want %q", k, got.Get(k), want.Get(k))
		}
	}
}

func TestVerifyJSONRejects(t *testing.T) {
	const base = "https://hooks.example.com/twilio/sms"
	body := []byte(`{"MessageSid":"SM1","Body":"hi"}`)
	hash := bodyHash(body)
	u := base + "?bodySHA256=" + hash
	good := signURL("token", u)
	if _, err := twilio.VerifyJSON([]byte("token"), u, jsonType, body, good); err != nil {
		t.Fatalf("valid request = %v", err)
	}
	other := []byte(`{"MessageSid":"SM1","Body":"evil"}`)
	otherURL := base + "?bodySHA256=" + bodyHash(other)
	badJSON := []byte(`{"MessageSid":`)
	badJSONURL := base + "?bodySHA256=" + bodyHash(badJSON)
	arrayBody := []byte(`["a"]`)
	arrayURL := base + "?bodySHA256=" + bodyHash(arrayBody)
	zURL := base + "?bodySHA256=" + strings.Repeat("z", 64)
	cases := []struct {
		name, token, url, ctype string
		body                    []byte
		sig                     string
		want                    error
	}{
		{"missing signature", "token", u, jsonType, body, "", twilio.ErrSignature},
		{"wrong token", "other", u, jsonType, body, good, twilio.ErrSignature},
		{"no token", "", u, jsonType, body, good, twilio.ErrSignature},
		// The signature covers the URL and so the hash, not the body: another
		// body under the same URL must fail the hash.
		{"tampered body", "token", u, jsonType, other, good, twilio.ErrSignature},
		{"hash of another body", "token", otherURL, jsonType, body, signURL("token", otherURL), twilio.ErrSignature},
		{"other url", "token", u + "&x=1", jsonType, body, good, twilio.ErrSignature},
		// A signature of a URL without the hash must not authenticate any body.
		{"no bodySHA256", "token", base, jsonType, body, signURL("token", base), twilio.ErrMalformed},
		{"empty bodySHA256", "token", base + "?bodySHA256=", jsonType, body, signURL("token", base+"?bodySHA256="), twilio.ErrMalformed},
		{"short bodySHA256", "token", base + "?bodySHA256=abcd", jsonType, body, signURL("token", base+"?bodySHA256=abcd"), twilio.ErrMalformed},
		{"non-hex bodySHA256", "token", zURL, jsonType, body, signURL("token", zURL), twilio.ErrMalformed},
		{"repeated bodySHA256", "token", u + "&bodySHA256=" + hash, jsonType, body, signURL("token", u+"&bodySHA256="+hash), twilio.ErrMalformed},
		{"form content type", "token", u, formType, body, good, twilio.ErrMalformed},
		{"unparseable URL", "token", "://bad", jsonType, body, good, twilio.ErrMalformed},
		{"invalid JSON", "token", badJSONURL, jsonType, badJSON, signURL("token", badJSONURL), twilio.ErrMalformed},
		{"JSON array", "token", arrayURL, jsonType, arrayBody, signURL("token", arrayURL), twilio.ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := twilio.VerifyJSON([]byte(c.token), c.url, c.ctype, c.body, c.sig)
			if !errors.Is(err, c.want) || got != nil {
				t.Fatalf("VerifyJSON = %v, %v; want %v and no fields", got, err, c.want)
			}
		})
	}
}

// Verify routes by content type and refuses any other.
func TestVerifyDispatch(t *testing.T) {
	const u = "https://hooks.example.com/twilio/sms"
	form := url.Values{"MessageSid": {"SM1"}, "Body": {"hi"}}
	body := []byte(form.Encode())
	if got, err := twilio.Verify([]byte("token"), u, formType, body, sign("token", u, form)); err != nil || got.Get("Body") != "hi" {
		t.Fatalf("Verify(form) = %v, %v", got, err)
	}
	for _, ct := range []string{"text/plain", "", "application/xml"} {
		if got, err := twilio.Verify([]byte("token"), u, ct, body, sign("token", u, form)); !errors.Is(err, twilio.ErrMalformed) || got != nil {
			t.Fatalf("Verify(%q) = %v, %v; want ErrMalformed", ct, got, err)
		}
	}
}
