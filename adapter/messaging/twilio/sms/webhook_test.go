package sms_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Twilio's documented webhook signature algorithm
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// Webhook tests post golden Twilio webhook bodies (testdata/webhooks, in
// the parameter sets of https://www.twilio.com/docs/messaging/guides/webhook-request
// and https://www.twilio.com/docs/messaging/guides/track-outbound-message-status),
// signed in-test with a fake auth token.

const hookTenant model.TenantID = "twilio-hooks"

const (
	hookAccount = "AC00000000000000000000000000000001"
	hookToken   = "fake-auth-token" // #nosec G101 -- fixture value, not a credential
	hookURL     = "https://hooks.example.com/twilio/sms/ch1"
	formType    = "application/x-www-form-urlencoded"
)

var (
	hookChannel = model.ChannelRef{Tenant: hookTenant, ID: "00000000-0000-7000-8000-0000000002a0"}
	hookInbox   = model.InboxRef{Tenant: hookTenant, ID: "00000000-0000-7000-8000-0000000002b1"}
)

func hookConfig() sms.Config {
	return sms.Config{AccountSID: hookAccount, From: senderNumber, WebhookURL: hookURL}
}

// signTwilio computes X-Twilio-Signature independently of the adapter.
func signTwilio(token, rawURL string, body []byte) string {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		panic(err)
	}
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

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name)) // #nosec G304 -- fixed fixture names
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tokenHost resolves the fake auth token for every purpose and counts
// resolutions.
type tokenHost struct {
	mu   sync.Mutex
	reqs []ports.CredentialRequest
	fail error
}

func (h *tokenHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.fail != nil {
		return nil, h.fail
	}
	return ports.Secret{Bytes: []byte(hookToken)}, nil
}

type hookHost struct {
	t      *testing.T
	ctx    context.Context
	engine uma.Engine
	store  *memory.Store
}

func newHookHost(t *testing.T, cfg sms.Config) *hookHost { return newHookHostOn(t, cfg, nil) }

// newHookHostOn is newHookHost whose engine reaches fake's Messages API;
// nil gives it no HTTP client.
func newHookHostOn(t *testing.T, cfg sms.Config, fake *fakeTwilio) *hookHost {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(1_760_000_000, 0))
	h := &hookHost{t: t, ctx: t.Context(), store: memory.New(memory.Options{Clock: clock})}
	opts, ucfg := sms.Options{}, uma.Config{}
	if fake != nil {
		opts.BaseURL, ucfg.HTTP = fake.srv.URL, fake.client()
	}
	ucfg.Store = h.store
	ucfg.Authorizer = policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
		return policy.Decision{Allow: true}, nil
	})
	ucfg.Adapters, ucfg.Credentials = []adapter.Factory{sms.NewFactory(opts)}, &tokenHost{}
	ucfg.Clock, ucfg.IDs = clock, portstest.NewSequenceIDs(11)
	engine, err := uma.New(ucfg)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	err = h.store.Transact(h.ctx, hookTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(h.ctx, model.Channel{
			Ref: hookChannel, Type: model.TypeTwilioSms, Configuration: cfg.Extension(), CredentialRef: "twilio", Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(h.ctx, model.Inbox{
			Ref: hookInbox, Channel: hookChannel, Name: "sms", Destinations: []string{senderNumber}, Selection: sel, Revision: "in1",
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *hookHost) handler() http.Handler {
	h.t.Helper()
	hd, err := h.engine.Runtime().Webhook(h.ctx, uma.RuntimeCall{
		Principal: model.Principal{Tenant: hookTenant, Actor: "hooks"}, Call: model.Call{Correlation: "hooks"},
	}, hookChannel)
	if err != nil {
		h.t.Fatalf("Webhook: %v", err)
	}
	return hd
}

func (h *hookHost) messages() []model.Message {
	h.t.Helper()
	var out []model.Message
	err := h.store.View(h.ctx, hookTenant, func(r store.Reader) error {
		p, err := r.Messages(h.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{hookInbox}}, Page: model.PageRequest{Limit: 50}})
		out = p.Items
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *hookHost) contacts() int {
	h.t.Helper()
	var n int
	err := h.store.View(h.ctx, hookTenant, func(r store.Reader) error {
		p, err := r.Contacts(h.ctx, model.ListRequest{Page: model.PageRequest{Limit: 50}})
		n = len(p.Items)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// post serves one request. The handler is mounted under a host-internal
// path; the signature covers the public hookURL plus query.
func post(hd http.Handler, query string, body []byte, ctype, sig string) *httptest.ResponseRecorder {
	target := "http://10.0.0.1:8080/internal/sms"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	if sig != "" {
		req.Header.Set("X-Twilio-Signature", sig)
	}
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	return rec
}

func TestWebhookIncomingMessageCommitsOnce(t *testing.T) {
	h := newHookHost(t, hookConfig())
	hd := h.handler()
	body := fixture(t, "incoming_sms.form")
	sig := signTwilio(hookToken, hookURL, body)
	for i := range 2 {
		rec := post(hd, "", body, formType, sig)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d = %d %q", i+1, rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/xml" || !strings.Contains(rec.Body.String(), "<Response></Response>") {
			t.Fatalf("answer %q %q, want empty TwiML", ct, rec.Body)
		}
	}
	ms := h.messages()
	if len(ms) != 1 {
		t.Fatalf("messages = %d, want 1 after a redelivery", len(ms))
	}
	m := ms[0]
	if a := m.Author.External; a.ID != "+15558675310" || a.Namespace != sms.Namespace || a.Kind != sms.RouteKind || a.Channel != hookChannel {
		t.Errorf("author %+v", a)
	}
	if len(m.Content.Parts) != 1 || m.Content.Parts[0].Text != "Hello from the other side" {
		t.Errorf("content %+v", m.Content)
	}
	if m.Source != model.TypeTwilioSms || m.Audience.Visibility != model.Public {
		t.Errorf("source %q audience %+v", m.Source, m.Audience)
	}
	var found bool
	for _, r := range m.External {
		found = found || r == model.ProviderMessageRef(hookChannel, "SM10000000000000000000000000000001")
	}
	if !found {
		t.Errorf("external refs %v lack the provider message SID", m.External)
	}
}

func TestWebhookUnverifiedRequestWritesNothing(t *testing.T) {
	h := newHookHost(t, hookConfig())
	hd := h.handler()
	body := fixture(t, "incoming_sms.form")
	good := signTwilio(hookToken, hookURL, body)
	otherAccount := bytes.Replace(body, []byte(hookAccount), []byte("AC00000000000000000000000000000002"), 1)
	cases := []struct {
		name, query, ctype, sig string
		body                    []byte
		want                    int
	}{
		{"missing signature", "", formType, "", body, http.StatusForbidden},
		{"wrong token", "", formType, signTwilio("other-token", hookURL, body), body, http.StatusForbidden},
		{"signed for the internal URL", "", formType, signTwilio(hookToken, "http://10.0.0.1:8080/internal/sms", body), body, http.StatusForbidden},
		{"query added", "operation=x", formType, good, body, http.StatusForbidden},
		{"tampered body", "", formType, good, bytes.Replace(body, []byte("Hello"), []byte("Howdy"), 1), http.StatusForbidden},
		{"repeated parameter", "", formType, good, append(append([]byte{}, body...), []byte("&Body=evil")...), http.StatusBadRequest},
		{"json body", "", "application/json", good, body, http.StatusBadRequest},
		{"other account, validly signed", "", formType, signTwilio(hookToken, hookURL, otherAccount), otherAccount, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(hd, c.query, c.body, c.ctype, c.sig)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if strings.TrimSpace(rec.Body.String()) != http.StatusText(c.want) {
				t.Fatalf("body %q, want the status text only", rec.Body)
			}
		})
	}
	req := httptest.NewRequest(http.MethodGet, "/internal/sms?"+string(body), nil)
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
	if n := len(h.messages()); n != 0 {
		t.Fatalf("messages = %d after unverified requests, want 0", n)
	}
	if n := h.contacts(); n != 0 {
		t.Fatalf("contacts = %d after unverified requests, want 0", n)
	}
}

func TestWebhookStatusCallbackStoresNoMessage(t *testing.T) {
	h := newHookHost(t, hookConfig())
	hd := h.handler()
	body := fixture(t, "status_delivered.form")
	q := "attempt=00000000-0000-7000-8000-0000000003a2&operation=00000000-0000-7000-8000-0000000003a1"
	rec := post(hd, q, body, formType, signTwilio(hookToken, hookURL+"?"+q, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status callback = %d %q", rec.Code, rec.Body)
	}
	if n := len(h.messages()); n != 0 {
		t.Fatalf("messages = %d after a status callback, want 0", n)
	}
}

func TestWebhookWithoutURLIsUnavailable(t *testing.T) {
	cfg := hookConfig()
	cfg.WebhookURL = ""
	h := newHookHost(t, cfg)
	body := fixture(t, "incoming_sms.form")
	if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// recordingSink records accepted ingress and answers err.
type recordingSink struct {
	mu  sync.Mutex
	got []model.Ingress
	err error
}

func (s *recordingSink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
	return model.IngressReceipt{Committed: true}, s.err
}

func (s *recordingSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "no interactions")
}

func openHook(t *testing.T, creds ports.Credentials) adapter.HTTPReceiver {
	t.Helper()
	inst, err := sms.NewFactory(sms.Options{}).Open(t.Context(), model.Channel{
		Ref: hookChannel, Type: model.TypeTwilioSms, Configuration: hookConfig().Extension(), CredentialRef: "twilio", Revision: "ch1",
	}, adapter.Dependencies{Credentials: creds, Clock: portstest.NewFixedClock(time.Unix(1_760_000_000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.(adapter.HTTPReceiver)
}

func TestWebhookStatusEnvelope(t *testing.T) {
	sink := &recordingSink{}
	creds := &tokenHost{}
	hd := openHook(t, creds).Handler(sink)
	const op, attempt = "00000000-0000-7000-8000-0000000003a1", "00000000-0000-7000-8000-0000000003a2"
	q := "attempt=" + attempt + "&operation=" + op
	for _, name := range []string{"status_delivered.form", "status_undelivered.form"} {
		body := fixture(t, name)
		if rec := post(hd, q, body, formType, signTwilio(hookToken, hookURL+"?"+q, body)); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", name, rec.Code)
		}
	}
	if len(sink.got) != 2 {
		t.Fatalf("sink got %d, want 2", len(sink.got))
	}
	for i, want := range []sms.Delivery{
		{Status: "delivered", State: model.Delivered, Operation: op, Attempt: attempt},
		{Status: "undelivered", State: model.Failed, ErrorCode: 30003, Operation: op, Attempt: attempt},
	} {
		env := sink.got[i].Envelope
		if env.Kind != model.EnvelopeStatus || env.MessageID != "SM20000000000000000000000000000001" ||
			env.EventID != env.MessageID+":"+want.Status || env.Destination != senderNumber || !env.Sender.IsZero() {
			t.Errorf("envelope %d = %+v", i, env)
		}
		wantRefs := []model.ExternalRef{
			model.ProviderMessageRef(hookChannel, env.MessageID),
			model.EchoRef(hookChannel, model.OperationRef{Tenant: hookChannel.Tenant, ID: op}, attempt),
		}
		if !slices.Equal(env.References, wantRefs) {
			t.Errorf("envelope %d references %v, want %v", i, env.References, wantRefs)
		}
		if d := env.Delivery; d == nil || d.State != want.State || (want.State == model.Failed) != (d.Failure != nil) {
			t.Errorf("envelope %d delivery report %+v, want state %s", i, d, want.State)
		}
		var got sms.Delivery
		if env.Payload.Name != sms.PayloadDelivery || json.Unmarshal(env.Payload.Data, &got) != nil || got != want {
			t.Errorf("envelope %d payload %s %s, want %+v", i, env.Payload.Name, env.Payload.Data, want)
		}
	}
	for _, r := range creds.reqs {
		if r.Purpose != sms.PurposeWebhook || r.Channel != hookChannel || r.Reference != "twilio" {
			t.Errorf("credential request %+v", r)
		}
	}
}

func TestWebhookMediaMessage(t *testing.T) {
	sink := &recordingSink{}
	hd := openHook(t, &tokenHost{}).Handler(sink)
	body := fixture(t, "incoming_mms.form")
	if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	parts := sink.got[0].Envelope.Content.Parts
	if len(parts) != 1 || parts[0].Kind != model.PartStructured || parts[0].Structured.Name != sms.PayloadMedia {
		t.Fatalf("parts = %+v, want one media part", parts)
	}
	var m sms.Media
	if err := json.Unmarshal(parts[0].Structured.Data, &m); err != nil || m.Count != 2 || len(m.ContentTypes) != 2 || m.ContentTypes[1] != "image/png" {
		t.Fatalf("media = %+v, %v", m, err)
	}
	if bytes.Contains(parts[0].Structured.Data, []byte("api.twilio.com")) {
		t.Fatal("media part keeps media URLs")
	}
}

func TestWebhookFailureAnswers(t *testing.T) {
	body := fixture(t, "incoming_sms.form")
	sig := signTwilio(hookToken, hookURL, body)
	for _, c := range []struct {
		name     string
		sinkErr  error
		credErr  error
		want     int
		accepted int
	}{
		{"retryable sink failure", model.NewError(model.Unavailable, "busy"), nil, http.StatusServiceUnavailable, 1},
		{"permanent sink failure", model.NewError(model.Validation, "bad"), nil, http.StatusUnprocessableEntity, 1},
		{"token unavailable", nil, model.NewError(model.Unavailable, "vault down"), http.StatusServiceUnavailable, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &recordingSink{err: c.sinkErr}
			hd := openHook(t, &tokenHost{fail: c.credErr}).Handler(sink)
			if rec := post(hd, "", body, formType, sig); rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if len(sink.got) != c.accepted {
				t.Fatalf("sink calls = %d, want %d", len(sink.got), c.accepted)
			}
		})
	}
	sink := &recordingSink{}
	recv := openHook(t, &tokenHost{})
	hd := recv.Handler(sink)
	_ = recv.(adapter.Instance).Close(context.Background())
	if rec := post(hd, "", body, formType, sig); rec.Code != http.StatusServiceUnavailable || len(sink.got) != 0 {
		t.Fatalf("closed instance = %d with %d sink calls", rec.Code, len(sink.got))
	}
}

// The status callback URL Send hands Twilio carries the attempt, and a
// callback Twilio signs over that full URL verifies and names the attempt.
func TestStatusCallbackFromSendNamesAttempt(t *testing.T) {
	fake := newFakeTwilio(t)
	req := sendReq()
	out, err := openSender(t, fake, 5*time.Second, &tokenHost{}).Send(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := url.Parse(fake.calls(hookAccount)[0].Form.Get("StatusCallback"))
	if err != nil || cb.Query().Get("attempt") != string(req.Attempt) {
		t.Fatalf("status callback %v lacks the attempt %s", cb, req.Attempt)
	}
	form := url.Values{
		"AccountSid": {hookAccount}, "From": {senderNumber}, "To": {req.Route.ID},
		"MessageSid": {out.References[0].ID}, "MessageStatus": {"sent"},
		"SmsSid": {out.References[0].ID}, "SmsStatus": {"sent"},
	}
	body := []byte(form.Encode())
	sink := &recordingSink{}
	hd := openHook(t, &tokenHost{}).Handler(sink)
	if rec := post(hd, cb.RawQuery, body, formType, signTwilio(hookToken, cb.String(), body)); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d", rec.Code)
	}
	var d sms.Delivery
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
