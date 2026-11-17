package httpmail_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

var (
	tenant = model.TenantID("httpmail-test")
	chRef  = model.ChannelRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-00000000000a"}
	now    = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
)

// token is the shared secret of the relay.
const token = "relay-token-0123456789abcdef" // #nosec G101 -- test fixture

// relayTokenForSigning is the shared secret as the HMAC key.
const relayTokenForSigning = token

func secretOf(s string) ports.Credential { return ports.Secret{Bytes: []byte(s)} }

// credHost is the host's credential port: one secret, every request noted.
type credHost struct {
	mu   sync.Mutex
	reqs []ports.CredentialRequest
	cred ports.Credential
	err  error
}

func newCredHost() *credHost { return &credHost{cred: ports.Secret{Bytes: []byte(token)}} }

func (h *credHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.err != nil {
		return nil, h.err
	}
	return h.cred, nil
}

func (h *credHost) requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.CredentialRequest(nil), h.reqs...)
}

// captureSink records every envelope the adapter hands over.
type captureSink struct {
	mu  sync.Mutex
	got []model.Ingress
	err error
}

func (s *captureSink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return model.IngressReceipt{}, s.err
	}
	s.got = append(s.got, in)
	return model.IngressReceipt{Committed: true}, nil
}

func (s *captureSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, errors.New("not used")
}

func (s *captureSink) ingresses() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.got...)
}

func (s *captureSink) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// baseConfig accepts support@example.com and sales@example.com under a
// bearer token.
func baseConfig() httpmail.Config {
	return httpmail.Config{
		Recipients: []httpmail.Recipient{
			{Address: "support@example.com", Destination: "support"},
			{Address: "sales@example.com", Destination: "sales"},
		},
		Auth: httpmail.Auth{Scheme: "bearer"},
	}
}

func channelOf(cfg httpmail.Config) model.Channel {
	return model.Channel{Ref: chRef, Type: model.TypeHttpMail, Configuration: cfg.Extension(), CredentialRef: "relay-cred", Revision: "ch1"}
}

// rig is one opened instance with its handler.
type rig struct {
	t     *testing.T
	h     http.Handler
	sink  *captureSink
	creds *credHost
	inst  adapter.Instance
}

func newRig(t *testing.T, cfg httpmail.Config, opts ...func(*httpmail.Options)) *rig {
	t.Helper()
	var o httpmail.Options
	for _, f := range opts {
		f(&o)
	}
	f := httpmail.NewFactory(o)
	creds := newCredHost()
	inst, err := f.Open(t.Context(), channelOf(cfg), adapter.Dependencies{Credentials: creds, Clock: fixedClock{now}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	sink := &captureSink{}
	return &rig{t: t, h: inst.(adapter.HTTPReceiver).Handler(sink), sink: sink, creds: creds, inst: inst}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// request is one POST to the webhook; Authorization carries the token
// unless the test sets another.
type request struct {
	contentType string
	body        string
	header      http.Header
	noAuth      bool
}

func (r *rig) do(rq request) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/mail", strings.NewReader(rq.body))
	if rq.contentType != "" {
		req.Header.Set("Content-Type", rq.contentType)
	}
	if !rq.noAuth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range rq.header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// postJSON posts body as application/json.
func (r *rig) postJSON(body string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.do(request{contentType: "application/json", body: body})
}

// postRaw posts a message as message/rfc822, lines ended with CRLF.
func (r *rig) postRaw(msg string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.do(request{contentType: "message/rfc822", body: strings.ReplaceAll(msg, "\n", "\r\n")})
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}
