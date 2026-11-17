package whatsapp_test

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
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

// Webhook tests post golden Twilio WhatsApp webhook bodies
// (testdata/webhooks, in the parameter sets of
// https://www.twilio.com/docs/messaging/guides/webhook-request and
// https://www.twilio.com/docs/messaging/guides/track-outbound-message-status),
// signed in-test with a fake auth token.

const (
	hookAccount  = "AC00000000000000000000000000000001"
	hookToken    = "fake-auth-token" // #nosec G101 -- fixture value, not a credential
	hookURL      = "https://hooks.example.com/twilio/whatsapp/ch1"
	formType     = "application/x-www-form-urlencoded"
	senderNumber = "+14155238886"
	userNumber   = "+15558675310"
)

var testNow = time.Unix(1_760_000_000, 0).UTC()

var (
	testChannel = model.ChannelRef{Tenant: "twilio-whatsapp", ID: "00000000-0000-7000-8000-0000000007a0"}
	testRoute   = model.ExternalRef{Channel: testChannel, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: userNumber}
	testOp      = model.OperationRef{Tenant: "twilio-whatsapp", ID: "00000000-0000-7000-8000-0000000007b1"}
)

func hookConfig() whatsapp.Config {
	return whatsapp.Config{AccountSID: hookAccount, From: senderNumber, WebhookURL: hookURL}
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

// tokenHost resolves the fake auth token for every purpose and records
// the requests.
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

func (h *tokenHost) requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.CredentialRequest(nil), h.reqs...)
}

// protocol is an in-memory adapter.ProtocolState.
type protocol struct {
	mu   sync.Mutex
	vals map[string]model.Extension
	revs map[string]model.Revision
	n    int
	fail error
}

func newProtocol() *protocol {
	return &protocol{vals: map[string]model.Extension{}, revs: map[string]model.Revision{}}
}

func (p *protocol) Load(_ context.Context, key string) (model.Extension, model.Revision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return model.Extension{}, "", p.fail
	}
	v, ok := p.vals[key]
	if !ok {
		return model.Extension{}, "", model.NewError(model.NotFound, "no state")
	}
	return v, p.revs[key], nil
}

func (p *protocol) CompareAndSwap(_ context.Context, key string, expected model.Revision, v model.Extension) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	if p.revs[key] != expected {
		return model.NewError(model.Conflict, "revision moved")
	}
	p.n++
	p.vals[key], p.revs[key] = v, model.Revision("r"+strconv.Itoa(p.n))
	return nil
}

// setLastInbound stores user's window as last seen at.
func (p *protocol) setLastInbound(user string, at time.Time) {
	data, _ := json.Marshal(map[string]int64{"last_inbound": at.Unix()})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	p.vals["window/"+user] = model.Extension{Name: whatsapp.WindowSchema, Version: whatsapp.WindowVersion, Data: data}
	p.revs["window/"+user] = model.Revision("r" + strconv.Itoa(p.n))
}

// lastInbound returns user's recorded window start, zero without one.
func (p *protocol) lastInbound(user string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var w struct {
		LastInbound int64 `json:"last_inbound"`
	}
	if json.Unmarshal(p.vals["window/"+user].Data, &w) != nil || w.LastInbound == 0 {
		return time.Time{}
	}
	return time.Unix(w.LastInbound, 0).UTC()
}

// open opens a channel instance on fake (nil: no HTTP client).
func open(t *testing.T, fake *fakeTwilio, cfg whatsapp.Config, deps adapter.Dependencies) adapter.Instance {
	t.Helper()
	opts := whatsapp.Options{Timeout: 5 * time.Second}
	if fake != nil {
		deps.HTTP, opts.BaseURL = fake.client(), fake.srv.URL
	}
	if deps.Clock == nil {
		deps.Clock = portstest.NewFixedClock(testNow)
	}
	inst, err := whatsapp.NewFactory(opts).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: model.TypeTwilioWhatsapp, Configuration: cfg.Extension(), CredentialRef: "twilio", Revision: "ch1",
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func sendReq(parts ...model.Part) adapter.SendRequest {
	if len(parts) == 0 {
		parts = []model.Part{{Kind: model.PartText, Text: "hello"}}
	}
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: testOp, Attempt: "00000000-0000-7000-8000-0000000007c1",
			Route: testRoute, Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: parts},
	}
}

var helloTemplate = whatsapp.Template{ContentSID: "HX00000000000000000000000000000001", Variables: map[string]string{"1": "Ada"}}

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

// post serves one request. The handler is mounted under a host-internal
// path; the signature covers the public hookURL plus query.
func post(hd http.Handler, query string, body []byte, ctype, sig string) *httptest.ResponseRecorder {
	target := "http://10.0.0.1:8080/internal/whatsapp"
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
