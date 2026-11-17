package whatsapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

func hook(t *testing.T, creds ports.Credentials, p adapter.ProtocolState, sink adapter.Sink) http.Handler {
	t.Helper()
	deps := adapter.Dependencies{Credentials: creds}
	if p != nil {
		deps.Protocol = p
	}
	return open(t, nil, hookConfig(), deps).(adapter.HTTPReceiver).Handler(sink)
}

func TestWebhookIncomingText(t *testing.T) {
	sink := &recordingSink{}
	p := newProtocol()
	hd := hook(t, &tokenHost{}, p, sink)
	body := fixture(t, "incoming_text.form")
	rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/xml" || !strings.Contains(rec.Body.String(), "<Response></Response>") {
		t.Fatalf("answer %q %q, want empty TwiML", ct, rec.Body)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d", len(sink.got))
	}
	env := sink.got[0].Envelope
	if env.Kind != model.EnvelopeMessage || env.MessageID != "SM10000000000000000000000000000001" || env.EventID != env.MessageID ||
		env.Destination != senderNumber || env.Assurance != "verified" || env.Audience.Visibility != model.Public {
		t.Fatalf("envelope %+v", env)
	}
	if want := (model.ExternalRef{Channel: testChannel, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: userNumber}); env.Sender != want {
		t.Fatalf("sender %+v, want %+v (no whatsapp: prefix)", env.Sender, want)
	}
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != "Hello from WhatsApp" {
		t.Fatalf("content %+v", env.Content)
	}
	var in whatsapp.Inbound
	if env.Payload.Name != whatsapp.PayloadInbound || json.Unmarshal(env.Payload.Data, &in) != nil ||
		in != (whatsapp.Inbound{ProfileName: "Ada", WaID: "15558675310", MessageType: "text"}) {
		t.Fatalf("payload %s %s", env.Payload.Name, env.Payload.Data)
	}
	// The verified message opened the sender's window at receipt time.
	if got := p.lastInbound(userNumber); !got.Equal(testNow) {
		t.Fatalf("window = %v, want %v", got, testNow)
	}
}

// A verified inbound message opens the window a later free-form send
// needs; before any recorded inbound message Twilio decides.
func TestInboundOpensWindowForSend(t *testing.T) {
	fake := newFakeTwilio(t)
	p := newProtocol()
	clock := portstest.NewFixedClock(testNow)
	inst := open(t, fake, hookConfig(), adapter.Dependencies{Credentials: &tokenHost{}, Protocol: p, Clock: clock})
	s := inst.(adapter.Sender)
	if out, err := s.Send(t.Context(), sendReq()); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send before any recorded inbound = %+v, %v; want it left to Twilio", out, err)
	}
	body := fixture(t, "incoming_text.form")
	if rec := post(inst.(adapter.HTTPReceiver).Handler(&recordingSink{}), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	clock.Advance(23 * time.Hour)
	if out, err := s.Send(t.Context(), sendReq()); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send 23h after inbound = %+v, %v; want provider_accepted", out, err)
	}
	clock.Advance(time.Hour)
	if out, err := s.Send(t.Context(), sendReq()); err != nil || out.State != model.Rejected {
		t.Fatalf("Send 24h after inbound = %+v, %v; want rejected", out, err)
	}
	if n := len(fake.calls(hookAccount)); n != 2 {
		t.Fatalf("calls = %d, want the unrecorded and the in-window sends", n)
	}
}

func TestWebhookOtherMessageTypes(t *testing.T) {
	cases := map[string]struct {
		fixture string
		texts   []string
		payload string
		check   func(t *testing.T, data []byte)
	}{
		"button": {"incoming_button.form", []string{"Confirm"}, whatsapp.PayloadButton, func(t *testing.T, data []byte) {
			var b whatsapp.Button
			if json.Unmarshal(data, &b) != nil || b != (whatsapp.Button{Payload: "confirm_booking_42", Text: "Confirm"}) {
				t.Fatalf("button %s", data)
			}
		}},
		"location": {"incoming_location.form", nil, whatsapp.PayloadLocation, func(t *testing.T, data []byte) {
			var l whatsapp.Location
			if json.Unmarshal(data, &l) != nil || l.Latitude != "37.7879277" || l.Longitude != "-122.4060772" || l.Label != "Twilio" {
				t.Fatalf("location %s", data)
			}
		}},
		"image": {"incoming_image.form", []string{"A caption"}, whatsapp.PayloadMedia, func(t *testing.T, data []byte) {
			var m whatsapp.Media
			if json.Unmarshal(data, &m) != nil || m.Count != 1 || len(m.ContentTypes) != 1 || m.ContentTypes[0] != "image/jpeg" {
				t.Fatalf("media %s", data)
			}
			if bytes.Contains(data, []byte("api.twilio.com")) {
				t.Fatal("media part keeps media URLs")
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			sink := &recordingSink{}
			body := fixture(t, c.fixture)
			if rec := post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			parts := sink.got[0].Envelope.Content.Parts
			if len(parts) != len(c.texts)+1 {
				t.Fatalf("parts = %+v", parts)
			}
			for k, txt := range c.texts {
				if parts[k].Kind != model.PartText || parts[k].Text != txt {
					t.Fatalf("part %d = %+v", k, parts[k])
				}
			}
			last := parts[len(parts)-1]
			if last.Kind != model.PartStructured || last.Structured.Name != c.payload {
				t.Fatalf("structured part %+v", last)
			}
			c.check(t, last.Structured.Data)
		})
	}
	t.Run("reply context", func(t *testing.T) {
		sink := &recordingSink{}
		body := fixture(t, "incoming_button.form")
		post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body))
		var in whatsapp.Inbound
		if json.Unmarshal(sink.got[0].Envelope.Payload.Data, &in) != nil || in.RepliedTo != "SM20000000000000000000000000000009" || in.MessageType != "button" {
			t.Fatalf("inbound %s", sink.got[0].Envelope.Payload.Data)
		}
		if got := sink.got[0].Envelope.InReplyTo; got != "SM20000000000000000000000000000009" {
			t.Fatalf("InReplyTo = %q, want the replied-to SID", got)
		}
	})
}

func TestWebhookUnverifiedRequestWritesNothing(t *testing.T) {
	body := fixture(t, "incoming_text.form")
	good := signTwilio(hookToken, hookURL, body)
	otherAccount := bytes.Replace(body, []byte(hookAccount), []byte("AC00000000000000000000000000000002"), 1)
	smsFrom := bytes.Replace(body, []byte("From=whatsapp%3A%2B"), []byte("From=%2B"), 1)
	cases := []struct {
		name, query, ctype, sig string
		body                    []byte
		want                    int
	}{
		{"missing signature", "", formType, "", body, http.StatusForbidden},
		{"wrong token", "", formType, signTwilio("other-token", hookURL, body), body, http.StatusForbidden},
		{"signed for the internal URL", "", formType, signTwilio(hookToken, "http://10.0.0.1:8080/internal/whatsapp", body), body, http.StatusForbidden},
		{"query added", "operation=x", formType, good, body, http.StatusForbidden},
		{"tampered body", "", formType, good, bytes.Replace(body, []byte("Hello"), []byte("Howdy"), 1), http.StatusForbidden},
		{"repeated parameter", "", formType, good, append(append([]byte{}, body...), []byte("&Body=evil")...), http.StatusBadRequest},
		{"json body", "", "application/json", good, body, http.StatusBadRequest},
		{"other account, validly signed", "", formType, signTwilio(hookToken, hookURL, otherAccount), otherAccount, http.StatusForbidden},
		{"SMS sender, validly signed", "", formType, signTwilio(hookToken, hookURL, smsFrom), smsFrom, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &recordingSink{}
			p := newProtocol()
			rec := post(hook(t, &tokenHost{}, p, sink), c.query, c.body, c.ctype, c.sig)
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
	req := httptest.NewRequest(http.MethodGet, "/internal/whatsapp?"+string(body), nil)
	rec := httptest.NewRecorder()
	hook(t, &tokenHost{}, nil, &recordingSink{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}

func TestWebhookStatusEnvelope(t *testing.T) {
	sink := &recordingSink{}
	creds := &tokenHost{}
	p := newProtocol()
	hd := hook(t, creds, p, sink)
	const op, attempt = "00000000-0000-7000-8000-0000000003a1", "00000000-0000-7000-8000-0000000003a2"
	q := "attempt=" + attempt + "&operation=" + op
	for _, name := range []string{"status_read.form", "status_window_closed.form"} {
		body := fixture(t, name)
		if rec := post(hd, q, body, formType, signTwilio(hookToken, hookURL+"?"+q, body)); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", name, rec.Code)
		}
	}
	if len(sink.got) != 2 {
		t.Fatalf("sink got %d, want 2", len(sink.got))
	}
	echo := model.EchoRef(testChannel, model.OperationRef{Tenant: testChannel.Tenant, ID: op}, attempt)
	for i, want := range []whatsapp.Delivery{
		{Status: "read", State: model.Delivered, Operation: op, Attempt: attempt},
		{Status: "undelivered", State: model.Failed, ErrorCode: whatsapp.CodeOutsideWindow, Operation: op, Attempt: attempt},
	} {
		env := sink.got[i].Envelope
		if env.Kind != model.EnvelopeStatus || env.MessageID != "SM20000000000000000000000000000001" ||
			env.EventID != env.MessageID+":"+want.Status || env.Destination != senderNumber || !env.Sender.IsZero() {
			t.Errorf("envelope %d = %+v", i, env)
		}
		if len(env.References) != 2 || env.References[0] != model.ProviderMessageRef(testChannel, env.MessageID) || env.References[1] != echo {
			t.Errorf("envelope %d references %v, want the SID and the attempt", i, env.References)
		}
		var got whatsapp.Delivery
		if env.Payload.Name != whatsapp.PayloadDelivery || json.Unmarshal(env.Payload.Data, &got) != nil || got != want {
			t.Errorf("envelope %d payload %s %s, want %+v", i, env.Payload.Name, env.Payload.Data, want)
		}
	}
	for _, r := range creds.requests() {
		if r.Purpose != whatsapp.PurposeWebhook || r.Channel != testChannel || r.Reference != "twilio" {
			t.Errorf("credential request %+v", r)
		}
	}
	if !p.lastInbound(userNumber).IsZero() {
		t.Fatal("a status callback opened the window")
	}
}

// A status callback without (or with a forged-looking) tag names no
// attempt: an attempt is never guessed.
func TestStatusEnvelopeNeverGuessesAnAttempt(t *testing.T) {
	sink := &recordingSink{}
	hd := hook(t, &tokenHost{}, nil, sink)
	body := fixture(t, "status_read.form")
	for _, q := range []string{"", "operation=not-an-id&attempt=00000000-0000-7000-8000-0000000003a2"} {
		signed := hookURL
		if q != "" {
			signed += "?" + q
		}
		if rec := post(hd, q, body, formType, signTwilio(hookToken, signed, body)); rec.Code != http.StatusOK {
			t.Fatalf("callback %q = %d", q, rec.Code)
		}
	}
	for i, in := range sink.got {
		if len(in.Envelope.References) != 1 {
			t.Errorf("envelope %d references %v, want the SID only", i, in.Envelope.References)
		}
	}
}

func TestWebhookFailureAnswers(t *testing.T) {
	body := fixture(t, "incoming_text.form")
	sig := signTwilio(hookToken, hookURL, body)
	failing := newProtocol()
	failing.fail = model.NewError(model.Unavailable, "store down")
	for _, c := range []struct {
		name     string
		sinkErr  error
		credErr  error
		protocol *protocol
		want     int
		accepted int
	}{
		{"retryable sink failure", model.NewError(model.Unavailable, "busy"), nil, nil, http.StatusServiceUnavailable, 1},
		{"permanent sink failure", model.NewError(model.Validation, "bad"), nil, nil, http.StatusUnprocessableEntity, 1},
		{"token unavailable", nil, model.NewError(model.Unavailable, "vault down"), nil, http.StatusServiceUnavailable, 0},
		{"window state unavailable", nil, nil, failing, http.StatusServiceUnavailable, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &recordingSink{err: c.sinkErr}
			var p adapter.ProtocolState
			if c.protocol != nil {
				p = c.protocol
			}
			if rec := post(hook(t, &tokenHost{fail: c.credErr}, p, sink), "", body, formType, sig); rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if len(sink.got) != c.accepted {
				t.Fatalf("sink calls = %d, want %d", len(sink.got), c.accepted)
			}
		})
	}
	sink := &recordingSink{}
	inst := open(t, nil, hookConfig(), adapter.Dependencies{Credentials: &tokenHost{}})
	hd := inst.(adapter.HTTPReceiver).Handler(sink)
	_ = inst.Close(context.Background())
	if rec := post(hd, "", body, formType, sig); rec.Code != http.StatusServiceUnavailable || len(sink.got) != 0 {
		t.Fatalf("closed instance = %d with %d sink calls", rec.Code, len(sink.got))
	}
	noURL := hookConfig()
	noURL.WebhookURL = ""
	hd = open(t, nil, noURL, adapter.Dependencies{Credentials: &tokenHost{}}).(adapter.HTTPReceiver).Handler(sink)
	if rec := post(hd, "", body, formType, sig); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no webhook URL = %d, want 503", rec.Code)
	}
}

// The status callback URL Send hands Twilio carries the attempt, and a
// callback Twilio signs over that full URL verifies and names the attempt.
func TestStatusCallbackFromSendNamesAttempt(t *testing.T) {
	fake := newFakeTwilio(t)
	req := sendReq()
	out, err := sender(t, fake, &tokenHost{}, nil).Send(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := url.Parse(fake.calls(hookAccount)[0].Form.Get("StatusCallback"))
	if err != nil || cb.Query().Get("attempt") != string(req.Attempt) {
		t.Fatalf("status callback %v lacks the attempt %s", cb, req.Attempt)
	}
	form := url.Values{
		"AccountSid": {hookAccount}, "From": {"whatsapp:" + senderNumber}, "To": {"whatsapp:" + userNumber},
		"MessageSid": {out.References[0].ID}, "MessageStatus": {"sent"}, "ChannelPrefix": {"whatsapp"},
		"SmsSid": {out.References[0].ID}, "SmsStatus": {"sent"},
	}
	body := []byte(form.Encode())
	sink := &recordingSink{}
	hd := hook(t, &tokenHost{}, nil, sink)
	if rec := post(hd, cb.RawQuery, body, formType, signTwilio(hookToken, cb.String(), body)); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d", rec.Code)
	}
	var d whatsapp.Delivery
	if err := json.Unmarshal(sink.got[0].Envelope.Payload.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Attempt != req.Attempt || d.Operation != req.Operation.ID || d.State != model.ProviderAccepted {
		t.Fatalf("delivery = %+v, want attempt %s of operation %s", d, req.Attempt, req.Operation.ID)
	}
	if rec := post(hd, cb.RawQuery, body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusForbidden {
		t.Fatalf("callback signed without its query = %d, want 403", rec.Code)
	}
}
