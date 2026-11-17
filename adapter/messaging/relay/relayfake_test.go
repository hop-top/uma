package relay_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// seen is one callback request as the fake relay received it.
type seen struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// fakeRelay is the operator's gateway: it takes callback POSTs and answers
// with whatever the test scripts.
type fakeRelay struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []seen
	// answer handles every request; the default accepts with an id.
	answer http.HandlerFunc
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, seen{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: body, Raw: raw})
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		answer(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"message_id":"gw-1"}`)
}

func (f *fakeRelay) script(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = h
}

func (f *fakeRelay) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

func (f *fakeRelay) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeRelay) factory(timeout time.Duration) *relay.Factory {
	return relay.NewFactory(relay.Options{CallbackHosts: []string{f.host()}, AllowHTTP: true, Timeout: timeout})
}

func (f *fakeRelay) config() relay.Config {
	c := baseConfig()
	c.Callback = &relay.Callback{URL: f.srv.URL + "/out"}
	return c
}

// rtClient is a host HTTP client that follows nothing and keeps no policy of
// its own: one round trip per Do.
type rtClient struct{ rt http.RoundTripper }

func (c rtClient) Do(r *http.Request) (*http.Response, error) { return c.rt.RoundTrip(r) }

func plainClient() ports.HTTPClient { return rtClient{http.DefaultTransport} }

// sendRig is an instance opened over a fake relay.
type sendRig struct {
	t     *testing.T
	fake  *fakeRelay
	creds *creds
	inst  *relay.Instance
}

func newSendRig(t *testing.T, mutate func(*relay.Config)) *sendRig {
	t.Helper()
	f := newFakeRelay(t)
	cfg := f.config()
	if mutate != nil {
		mutate(&cfg)
	}
	c := newCreds()
	inst := open(t, f.factory(0), cfg, adapter.Dependencies{Credentials: c, HTTP: plainClient()})
	return &sendRig{t: t, fake: f, creds: c, inst: inst}
}

func peerRoute(id string) model.ExternalRef {
	return model.ExternalRef{Channel: testChannel, Namespace: relay.NamespaceSMS, Kind: relay.RouteKind, ID: id}
}

const (
	testOp      model.ID = "00000000-0000-7000-8000-0000000000b1"
	testAttempt model.ID = "00000000-0000-7000-8000-0000000000a1"
)

func sendReq(text string) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
			Route: peerRoute("+15550100001"), Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

func jsonAnswer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func textAnswer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}
