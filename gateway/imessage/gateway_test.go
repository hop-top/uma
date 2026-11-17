package imessage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// callbackSecret and webhookSecret are fixture values, not credentials.
const (
	callbackSecret = "gateway-callback-secret-0123456789" // #nosec G101
	webhookSecret  = "gateway-webhook-secret-0123456789"  // #nosec G101
)

// scriptedBackend answers each send with the next scripted result and
// counts the sends.
type scriptedBackend struct {
	mu      sync.Mutex
	results []Result
	sends   []Outbound
	ctxErrs []error
	block   chan struct{} // when set, Send waits for it
	signs   bool
	msgs    []Inbound
}

func (b *scriptedBackend) Name() string        { return "scripted" }
func (b *scriptedBackend) SignsWebhooks() bool { return b.signs }
func (b *scriptedBackend) VerifyWebhook(h http.Header, _ []byte) error {
	if b.signs && h.Get("X-Test-Signature") != "ok" {
		return errSignature
	}
	return nil
}
func (b *scriptedBackend) Messages([]byte) ([]Inbound, error) { return b.msgs, nil }

func (b *scriptedBackend) Send(ctx context.Context, m Outbound) Result {
	if b.block != nil {
		<-b.block
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends = append(b.sends, m)
	b.ctxErrs = append(b.ctxErrs, ctx.Err())
	if len(b.results) == 0 {
		return Result{Outcome: Sent, MessageID: "msg-default"}
	}
	r := b.results[0]
	b.results = b.results[1:]
	return r
}

func (b *scriptedBackend) sent() []Outbound {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Outbound(nil), b.sends...)
}

type rig struct {
	t    *testing.T
	be   *scriptedBackend
	keys *KeyStore
	gw   *Gateway
	h    http.Handler
	dir  string
	cfg  Config
}

func newRig(t *testing.T, be *scriptedBackend, mutate ...func(*Config)) *rig {
	t.Helper()
	dir := t.TempDir()
	keys, err := OpenKeyStore(dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close() })
	cfg := Config{
		Backend: be, Keys: keys, CallbackSecret: []byte(callbackSecret),
		WebhookURL: "http://uma.invalid/webhook", WebhookSecret: []byte(webhookSecret),
		WebhookToken: "path-token-0123456789",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, be: be, keys: keys, gw: gw, h: gw.Handler(), dir: dir, cfg: cfg}
}

// restart reopens the key store from disk and builds a new gateway on it.
func (r *rig) restart() {
	r.t.Helper()
	must(r.t, r.keys.Close())
	keys, err := OpenKeyStore(r.dir, 0, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = keys.Close() })
	r.cfg.Keys = keys
	gw, err := New(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	r.keys, r.gw, r.h = keys, gw, gw.Handler()
}

func callbackJSON(key string) string {
	return `{"medium":"imessage","to":"alice@example.com","body":"hi","operation_id":"` + key + `","attempt_id":"a-1"}`
}

func (r *rig) callback(key, body string, header ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, PathReply, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callbackSecret)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
			continue
		}
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *rig) lookup(key string, header ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, PathLookup+"?operation_id="+key, nil)
	req.Header.Set("Authorization", "Bearer "+callbackSecret)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("answer %d %q is not a JSON object: %v", rec.Code, rec.Body.String(), err)
	}
	return m
}

func TestCallbackSendsAndAnswersTheBackendID(t *testing.T) {
	r := newRig(t, &scriptedBackend{results: []Result{{Outcome: Sent, MessageID: "msg-1"}}})
	rec := r.callback("op-1", callbackJSON("op-1"))
	if rec.Code != http.StatusOK || jsonOf(t, rec)["message_id"] != "msg-1" {
		t.Fatalf("answer %d %s, want 200 with message_id msg-1", rec.Code, rec.Body)
	}
	sent := r.be.sent()
	if len(sent) != 1 || sent[0] != (Outbound{Key: "op-1", To: "alice@example.com", Text: "hi"}) {
		t.Fatalf("backend sends %+v", sent)
	}
}

// A callback with a key the gateway holds is the same reply: answered from
// the record, never sent again, before and after a restart.
func TestCallbackRepeatNeverSendsTwice(t *testing.T) {
	r := newRig(t, &scriptedBackend{results: []Result{{Outcome: Sent, MessageID: "msg-1"}}})
	first := r.callback("op-1", callbackJSON("op-1"))
	second := r.callback("op-1", strings.Replace(callbackJSON("op-1"), "a-1", "a-2", 1))
	r.restart()
	third := r.callback("op-1", callbackJSON("op-1"))
	for i, rec := range []*httptest.ResponseRecorder{first, second, third} {
		if rec.Code != http.StatusOK || jsonOf(t, rec)["message_id"] != "msg-1" {
			t.Errorf("callback %d: %d %s, want 200 msg-1", i+1, rec.Code, rec.Body)
		}
	}
	if n := len(r.be.sent()); n != 1 {
		t.Fatalf("%d backend sends, want 1", n)
	}
}

// The key is recorded before the backend is called: a second callback
// while the first is in flight is not sent, and is answered 409, which uma
// takes as outcome unknown.
func TestCallbackKeyRecordedBeforeSending(t *testing.T) {
	be := &scriptedBackend{block: make(chan struct{})}
	r := newRig(t, be)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- r.callback("op-1", callbackJSON("op-1")) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec, ok := r.keys.Get("op-1"); ok && rec.State == StatePending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("key never recorded while the send is in flight")
		}
		time.Sleep(time.Millisecond)
	}
	if rec := r.callback("op-1", callbackJSON("op-1")); rec.Code != http.StatusConflict {
		t.Errorf("concurrent repeat answered %d, want 409", rec.Code)
	}
	if rec := r.lookup("op-1"); rec.Code != http.StatusOK || jsonOf(t, rec)["status"] != "pending" {
		t.Errorf("lookup in flight answered %d %s, want pending", rec.Code, rec.Body)
	}
	close(be.block)
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("first callback answered %d", rec.Code)
	}
	if n := len(be.sent()); n != 1 {
		t.Fatalf("%d sends, want 1", n)
	}
}

// uma stopping to wait does not cut the backend call short: its outcome is
// still recorded.
func TestCallbackSendOutlivesTheRequest(t *testing.T) {
	r := newRig(t, &scriptedBackend{})
	req := httptest.NewRequest(http.MethodPost, PathReply, strings.NewReader(callbackJSON("op-1")))
	req.Header.Set("Authorization", "Bearer "+callbackSecret)
	req.Header.Set("Idempotency-Key", "op-1")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	r.h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	if errs := r.be.ctxErrs; len(errs) != 1 || errs[0] != nil {
		t.Fatalf("backend context errors %v, want a live context", errs)
	}
	if rec, ok := r.keys.Get("op-1"); !ok || rec.State != StateSent {
		t.Fatalf("record %+v %v, want sent", rec, ok)
	}
}

func TestCallbackOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		res        Result
		code       int
		retryAfter string
		// held: the key is kept (never sent again); else released.
		held       bool
		lookupCode int
		lookup     string
	}{
		{"sent", Result{Outcome: Sent, MessageID: "m-9"}, 200, "", true, 200, "sent"},
		{"rejected", Result{Outcome: Rejected}, 422, "", true, 200, "rejected"},
		{"unknown", Result{Outcome: Unknown}, 502, "", true, 200, "pending"},
		{"not sent", Result{Outcome: NotSent}, 503, "", false, 404, "not_found"},
		{"rate limited", Result{Outcome: NotSent, RateLimited: true, RetryAfter: 30 * time.Second}, 429, "30", false, 404, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, &scriptedBackend{results: []Result{tt.res}})
			rec := r.callback("op-1", callbackJSON("op-1"))
			if rec.Code != tt.code || rec.Header().Get("Retry-After") != tt.retryAfter {
				t.Fatalf("answer %d Retry-After %q, want %d %q", rec.Code, rec.Header().Get("Retry-After"), tt.code, tt.retryAfter)
			}
			lk := r.lookup("op-1")
			if lk.Code != tt.lookupCode || jsonOf(t, lk)["status"] != tt.lookup {
				t.Errorf("lookup %d %s, want %d %s", lk.Code, lk.Body, tt.lookupCode, tt.lookup)
			}
			again := r.callback("op-1", callbackJSON("op-1"))
			wantSends := 1
			if !tt.held {
				wantSends = 2
			} else if again.Code != tt.code {
				t.Errorf("repeat answered %d, want the same %d", again.Code, tt.code)
			}
			if n := len(r.be.sent()); n != wantSends {
				t.Errorf("%d sends after a repeat, want %d", n, wantSends)
			}
		})
	}
}

func TestCallbackRefusals(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		body   string
		header []string
		code   int
	}{
		{"no credential", "op-1", callbackJSON("op-1"), []string{"Authorization", ""}, 401},
		{"wrong credential", "op-1", callbackJSON("op-1"), []string{"Authorization", "Bearer nope"}, 401},
		{"not bearer", "op-1", callbackJSON("op-1"), []string{"Authorization", "Basic " + callbackSecret}, 401},
		{"no key", "", callbackJSON("op-1"), nil, 400},
		{"key differs from operation_id", "op-2", callbackJSON("op-1"), nil, 400},
		{"not JSON", "op-1", "hello", nil, 400},
		{"another medium", "op-1", strings.Replace(callbackJSON("op-1"), "imessage", "sms", 1), nil, 422},
		{"no text", "op-1", strings.Replace(callbackJSON("op-1"), `"hi"`, `" "`, 1), nil, 422},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, &scriptedBackend{})
			if rec := r.callback(tt.key, tt.body, tt.header...); rec.Code != tt.code {
				t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, tt.code)
			}
			if n := len(r.be.sent()); n != 0 {
				t.Errorf("%d sends", n)
			}
			if _, ok := r.keys.Get("op-1"); ok {
				t.Error("a refused callback recorded its key")
			}
		})
	}
}

// With callback.auth "header", the secret is the whole value of the named
// header, and a bearer token is not accepted instead.
func TestCallbackHeaderCredential(t *testing.T) {
	r := newRig(t, &scriptedBackend{}, func(c *Config) { c.CallbackHeader = "X-Gateway-Key" })
	if rec := r.callback("op-1", callbackJSON("op-1")); rec.Code != http.StatusUnauthorized {
		t.Errorf("bearer answered %d, want 401", rec.Code)
	}
	if rec := r.callback("op-1", callbackJSON("op-1"), "Authorization", "", "X-Gateway-Key", callbackSecret); rec.Code != http.StatusOK {
		t.Errorf("header answered %d, want 200", rec.Code)
	}
	if rec := r.lookup("op-1", "Authorization", "", "X-Gateway-Key", callbackSecret); rec.Code != http.StatusOK {
		t.Errorf("lookup with the header answered %d, want 200", rec.Code)
	}
}

func TestLookup(t *testing.T) {
	r := newRig(t, &scriptedBackend{results: []Result{{Outcome: Sent, MessageID: "msg-1"}}})
	if rec := r.lookup("op-1"); rec.Code != http.StatusNotFound || jsonOf(t, rec)["status"] != "not_found" {
		t.Errorf("never recorded: %d %s, want 404 not_found", rec.Code, rec.Body)
	}
	r.callback("op-1", callbackJSON("op-1"))
	r.restart()
	rec := r.lookup("op-1")
	if m := jsonOf(t, rec); rec.Code != http.StatusOK || m["status"] != "sent" || m["message_id"] != "msg-1" {
		t.Errorf("after a restart: %d %s, want sent msg-1", rec.Code, rec.Body)
	}
	if rec := r.lookup("op-1", "Authorization", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong credential: %d, want 401", rec.Code)
	}
	if rec := r.lookup(""); rec.Code != http.StatusBadRequest {
		t.Errorf("no operation: %d, want 400", rec.Code)
	}
}

func TestNewRefusesUnsafeConfigurations(t *testing.T) {
	keys, err := OpenKeyStore(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = keys.Close() }()
	good := Config{
		Backend: &scriptedBackend{signs: true}, Keys: keys, CallbackSecret: []byte(callbackSecret),
		WebhookURL: "http://uma.invalid/webhook", WebhookSecret: []byte(webhookSecret),
	}
	if _, err := New(good); err != nil {
		t.Fatalf("good configuration: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no callback secret":            func(c *Config) { c.CallbackSecret = nil },
		"callback secret with a space":  func(c *Config) { c.CallbackSecret = []byte("a b") },
		"short webhook secret":          func(c *Config) { c.WebhookSecret = []byte("short") },
		"no uma URL":                    func(c *Config) { c.WebhookURL = "" },
		"unsigned backend and no token": func(c *Config) { c.Backend = &scriptedBackend{} },
		"short token":                   func(c *Config) { c.Backend = &scriptedBackend{}; c.WebhookToken = "short" },
		"token with a slash":            func(c *Config) { c.WebhookToken = "aaaaaaaa/bbbbbbbbbb" },
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			mutate(&c)
			if _, err := New(c); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
