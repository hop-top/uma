package sms_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
)

const jsonType = "application/json"

// jsonOf is the JSON object of the fields of a golden form body, as a
// relay posting JSON would send them, and the query Twilio adds to the
// webhook URL for it: the hex SHA-256 of the raw body as bodySHA256.
func jsonOf(t *testing.T, form []byte, extra ...string) (body []byte, query string) {
	t.Helper()
	v, err := url.ParseQuery(string(form))
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]string{}
	for k := range v {
		obj[k] = v.Get(k)
	}
	if body, err = json.Marshal(obj); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return body, strings.Join(append(extra, "bodySHA256="+hex.EncodeToString(sum[:])), "&")
}

// signJSON signs a JSON webhook as Twilio documents: the HMAC-SHA1 of the
// full URL, bodySHA256 included, with no parameters. signTwilio is the
// independent signer of the form tests.
func signJSON(token, query string) string {
	return signTwilio(token, hookURL+"?"+query, nil)
}

// A JSON-bodied webhook with a valid bodySHA256 reports the same message
// as the form of the same fields, and a redelivery commits once.
func TestWebhookJSONIncomingMessage(t *testing.T) {
	h := newHookHost(t, hookConfig())
	hd := h.handler()
	body, q := jsonOf(t, fixture(t, "incoming_sms.form"))
	for i := range 2 {
		if rec := post(hd, q, body, jsonType, signJSON(hookToken, q)); rec.Code != http.StatusOK {
			t.Fatalf("delivery %d = %d %q", i+1, rec.Code, rec.Body)
		}
	}
	ms := h.messages()
	if len(ms) != 1 {
		t.Fatalf("messages = %d, want 1 after a redelivery", len(ms))
	}
	m := ms[0]
	if a := m.Author.External; a.ID != "+15558675310" || a.Namespace != sms.Namespace || a.Channel != hookChannel {
		t.Errorf("author %+v", a)
	}
	if len(m.Content.Parts) != 1 || m.Content.Parts[0].Text != "Hello from the other side" {
		t.Errorf("content %+v", m.Content)
	}
}

// A JSON body is verified before anything is read from it.
func TestWebhookJSONUnverifiedRequestWritesNothing(t *testing.T) {
	h := newHookHost(t, hookConfig())
	hd := h.handler()
	form := fixture(t, "incoming_sms.form")
	body, q := jsonOf(t, form)
	tampered := []byte(strings.Replace(string(body), "Hello", "Howdy", 1))
	otherForm := []byte(strings.Replace(string(form), hookAccount, "AC00000000000000000000000000000002", 1))
	otherBody, otherQ := jsonOf(t, otherForm)
	noAccount, noAccountQ := jsonOf(t, []byte(strings.Replace(string(form), "AccountSid", "Other", 1)))
	_, otherHashQ := jsonOf(t, []byte("Body=x"))
	cases := []struct {
		name, query string
		body        []byte
		sig         string
		want        int
	}{
		{"missing signature", q, body, "", http.StatusForbidden},
		{"wrong token", q, body, signJSON("other-token", q), http.StatusForbidden},
		{"tampered body", q, tampered, signJSON(hookToken, q), http.StatusForbidden},
		{"hash of another body", otherHashQ, body, signJSON(hookToken, otherHashQ), http.StatusForbidden},
		// A signature of the URL without the hash covers no body at all.
		{"no bodySHA256", "", body, signTwilio(hookToken, hookURL, nil), http.StatusBadRequest},
		{"other account, validly signed", otherQ, otherBody, signJSON(hookToken, otherQ), http.StatusForbidden},
		{"no account, validly signed", noAccountQ, noAccount, signJSON(hookToken, noAccountQ), http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(hd, c.query, c.body, jsonType, c.sig)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if strings.TrimSpace(rec.Body.String()) != http.StatusText(c.want) {
				t.Fatalf("body %q, want the status text only", rec.Body)
			}
		})
	}
	if n := len(h.messages()); n != 0 {
		t.Fatalf("messages = %d after unverified requests, want 0", n)
	}
	if n := h.contacts(); n != 0 {
		t.Fatalf("contacts = %d after unverified requests, want 0", n)
	}
}

// A JSON status callback reports its delivery like the form one, and the
// operation and attempt its URL is tagged with are read from the query
// beside bodySHA256.
func TestWebhookJSONStatusEnvelope(t *testing.T) {
	sink := &recordingSink{}
	hd := openHook(t, &tokenHost{}).Handler(sink)
	const op, attempt = "00000000-0000-7000-8000-0000000003a1", "00000000-0000-7000-8000-0000000003a2"
	body, q := jsonOf(t, fixture(t, "status_delivered.form"), "attempt="+attempt, "operation="+op)
	if rec := post(hd, q, body, jsonType, signJSON(hookToken, q)); rec.Code != http.StatusOK {
		t.Fatalf("status callback = %d %q", rec.Code, rec.Body)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d, want 1", len(sink.got))
	}
	env := sink.got[0].Envelope
	var d sms.Delivery
	if env.Kind != model.EnvelopeStatus || json.Unmarshal(env.Payload.Data, &d) != nil ||
		d.State != model.Delivered || d.Operation != op || d.Attempt != attempt {
		t.Fatalf("envelope %+v delivery %+v", env, d)
	}
}
