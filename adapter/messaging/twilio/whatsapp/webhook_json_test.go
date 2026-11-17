package whatsapp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/twilio/whatsapp"
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
// as the form of the same fields and opens the sender's window.
func TestWebhookJSONIncomingText(t *testing.T) {
	sink := &recordingSink{}
	p := newProtocol()
	hd := hook(t, &tokenHost{}, p, sink)
	body, q := jsonOf(t, fixture(t, "incoming_text.form"))
	if rec := post(hd, q, body, jsonType, signJSON(hookToken, q)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d", len(sink.got))
	}
	env := sink.got[0].Envelope
	if env.Kind != model.EnvelopeMessage || env.MessageID != "SM10000000000000000000000000000001" ||
		env.Destination != senderNumber || env.Sender.ID != userNumber {
		t.Fatalf("envelope %+v", env)
	}
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != "Hello from WhatsApp" {
		t.Fatalf("content %+v", env.Content)
	}
	var in whatsapp.Inbound
	if json.Unmarshal(env.Payload.Data, &in) != nil || in.ProfileName != "Ada" || in.WaID != "15558675310" {
		t.Fatalf("payload %s", env.Payload.Data)
	}
	if got := p.lastInbound(userNumber); !got.Equal(testNow) {
		t.Fatalf("window = %v, want %v", got, testNow)
	}
}

// A JSON body is verified before anything is read from it: nothing reaches
// the sink or opens a window.
func TestWebhookJSONUnverifiedRequestWritesNothing(t *testing.T) {
	form := fixture(t, "incoming_text.form")
	body, q := jsonOf(t, form)
	tampered := bytes.Replace(body, []byte("Hello"), []byte("Howdy"), 1)
	otherBody, otherQ := jsonOf(t, bytes.Replace(form, []byte(hookAccount), []byte("AC00000000000000000000000000000002"), 1))
	smsBody, smsQ := jsonOf(t, bytes.Replace(form, []byte("From=whatsapp%3A%2B"), []byte("From=%2B"), 1))
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
		{"SMS sender, validly signed", smsQ, smsBody, signJSON(hookToken, smsQ), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &recordingSink{}
			p := newProtocol()
			rec := post(hook(t, &tokenHost{}, p, sink), c.query, c.body, jsonType, c.sig)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if strings.TrimSpace(rec.Body.String()) != http.StatusText(c.want) {
				t.Fatalf("body %q, want the status text only", rec.Body)
			}
			if len(sink.got) != 0 || !p.lastInbound(userNumber).IsZero() {
				t.Fatalf("unverified request reached the sink (%d) or opened a window", len(sink.got))
			}
		})
	}
}

// A JSON status callback reports its delivery like the form one, and the
// operation and attempt its URL is tagged with are read from the query
// beside bodySHA256.
func TestWebhookJSONStatusEnvelope(t *testing.T) {
	sink := &recordingSink{}
	hd := hook(t, &tokenHost{}, newProtocol(), sink)
	const op, attempt = "00000000-0000-7000-8000-0000000003a1", "00000000-0000-7000-8000-0000000003a2"
	body, q := jsonOf(t, fixture(t, "status_read.form"), "attempt="+attempt, "operation="+op)
	if rec := post(hd, q, body, jsonType, signJSON(hookToken, q)); rec.Code != http.StatusOK {
		t.Fatalf("status callback = %d %q", rec.Code, rec.Body)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d, want 1", len(sink.got))
	}
	env := sink.got[0].Envelope
	var d whatsapp.Delivery
	if env.Kind != model.EnvelopeStatus || json.Unmarshal(env.Payload.Data, &d) != nil ||
		d.State != model.Delivered || d.Operation != op || d.Attempt != attempt {
		t.Fatalf("envelope %+v delivery %+v", env, d)
	}
}
