package google

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// pushEnv is an env with push configured and a stored historyId.
func pushEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, Config{}, pushConfig)
	if err := e.inst.saveState(t.Context(), receiverState{HistoryID: 1000}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) push(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.pushTo(t, &testSink{}, token, body)
}

func (e *env) pushTo(t *testing.T, sink *testSink, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/gmail", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.inst.Handler(sink).ServeHTTP(w, r)
	return w
}

func (e *env) token(t *testing.T, edit func(claims)) string {
	c := e.validClaims()
	if edit != nil {
		edit(c)
	}
	return signToken(t, e.g.key, "test-key", c)
}

func (e *env) gmailCalls() int { return e.g.count("GET /gmail/v1") + e.g.count("POST /gmail/v1") }

func TestPushIngestsAndWakes(t *testing.T) {
	e := pushEnv(t)
	e.inbound("m1", testPeer, "pushed")
	sink := &testSink{}
	w := e.pushTo(t, sink, e.token(t, nil), pushBody(testMailbox, 1001))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("accepted %v", got)
	}
	select {
	case <-e.inst.state.wake:
	default:
		t.Fatal("receiver not woken")
	}
	if st := e.st(t); st.HistoryID != 1000 {
		t.Fatalf("push handler advanced the stored historyId to %d", st.HistoryID)
	}
}

func TestPushAuthentication(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		token func(e *env) string
		want  int
	}{
		"missing":     {func(*env) string { return "" }, http.StatusUnauthorized},
		"garbage":     {func(*env) string { return "a.b.c" }, http.StatusForbidden},
		"foreign key": {func(e *env) string { return signToken(t, other, "test-key", e.validClaims()) }, http.StatusForbidden},
		"unknown kid": {func(e *env) string { return signToken(t, e.g.key, "other-kid", e.validClaims()) }, http.StatusForbidden},
		"audience":    {func(e *env) string { return e.token(t, func(c claims) { c["aud"] = "https://evil.test/" }) }, http.StatusForbidden},
		"issuer":      {func(e *env) string { return e.token(t, func(c claims) { c["iss"] = "https://evil.test" }) }, http.StatusForbidden},
		"service acct": {func(e *env) string {
			return e.token(t, func(c claims) { c["email"] = "other@x.iam.gserviceaccount.com" })
		}, http.StatusForbidden},
		"unverified": {func(e *env) string { return e.token(t, func(c claims) { c["email_verified"] = false }) }, http.StatusForbidden},
		"expired": {func(e *env) string {
			return e.token(t, func(c claims) { c["exp"] = e.clock.Now().Add(-2 * time.Minute).Unix() })
		}, http.StatusForbidden},
		"issued later": {func(e *env) string {
			return e.token(t, func(c claims) { c["iat"] = e.clock.Now().Add(time.Hour).Unix() })
		}, http.StatusForbidden},
		"issuer bare ok": {func(e *env) string { return e.token(t, func(c claims) { c["iss"] = "accounts.google.com" }) }, http.StatusNoContent},
		"aud array ok":   {func(e *env) string { return e.token(t, func(c claims) { c["aud"] = []string{"x", testAud} }) }, http.StatusNoContent},
		"alg none": {func(e *env) string {
			tok := e.token(t, nil)
			parts := strings.Split(tok, ".")
			return "eyJhbGciOiJub25lIiwia2lkIjoidGVzdC1rZXkifQ." + parts[1] + "."
		}, http.StatusForbidden},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := pushEnv(t)
			e.inbound("m1", testPeer, "x")
			sink := &testSink{}
			w := e.pushTo(t, sink, c.token(e), pushBody(testMailbox, 1001))
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d", w.Code, c.want)
			}
			if c.want != http.StatusNoContent {
				if len(sink.ids()) != 0 || e.gmailCalls() != 0 {
					t.Fatalf("rejected push reached Gmail (%q) or the sink (%v)", e.g.requests(), sink.ids())
				}
			}
		})
	}
}

func TestPushIgnoresOtherMailboxAndOldHistory(t *testing.T) {
	e := pushEnv(t)
	if w := e.push(t, e.token(t, nil), pushBody("someone@else.test", 5000)); w.Code != http.StatusNoContent {
		t.Fatalf("other mailbox: %d", w.Code)
	}
	if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 999)); w.Code != http.StatusNoContent {
		t.Fatalf("old history: %d", w.Code)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/history"); n != 0 {
		t.Fatalf("history calls = %d, want none", n)
	}
	if w := e.push(t, e.token(t, nil), `{"message":{"data":"!!"}}`); w.Code != http.StatusNoContent {
		t.Fatalf("undecodable data: %d", w.Code)
	}
}

func TestPushBeforeAnyReceiverRound(t *testing.T) {
	e := newEnv(t, Config{}, pushConfig)
	if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 1001)); w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/history"); n != 0 {
		t.Fatalf("history read without a stored historyId")
	}
}

func TestPushRetryableFailureIs503(t *testing.T) {
	e := pushEnv(t)
	e.inbound("m1", testPeer, "x")
	sink := &testSink{refuse: func(model.Ingress) error { return model.NewError(model.Unavailable, "down") }}
	if w := e.pushTo(t, sink, e.token(t, nil), pushBody(testMailbox, 1001)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for redelivery", w.Code)
	}
}

func TestPushHistoryGoneLeavesResyncToReceiver(t *testing.T) {
	e := pushEnv(t)
	e.g.mu.Lock()
	e.box.minHistory = 2000
	e.g.mu.Unlock()
	if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 2001)); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages?"); n != 0 {
		t.Fatal("push handler ran a full sync")
	}
}

func TestPushKeysCachedAndRefetchBounded(t *testing.T) {
	e := pushEnv(t)
	for range 3 {
		if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 999)); w.Code != http.StatusNoContent {
			t.Fatalf("status %d", w.Code)
		}
	}
	for range 3 {
		if w := e.push(t, signToken(t, e.g.key, "rotated", e.validClaims()), pushBody(testMailbox, 999)); w.Code != http.StatusForbidden {
			t.Fatalf("unknown kid status %d", w.Code)
		}
	}
	e.g.mu.Lock()
	hits := e.g.certHits
	e.g.mu.Unlock()
	if hits != 1 {
		t.Fatalf("certs fetches = %d, want 1 (cached, unknown kids refetch at most once a minute)", hits)
	}
	e.clock.Advance(2 * time.Minute)
	_ = e.push(t, signToken(t, e.g.key, "rotated", e.validClaims()), pushBody(testMailbox, 999))
	e.clock.Advance(time.Hour)
	_ = e.push(t, e.token(t, nil), pushBody(testMailbox, 999))
	e.g.mu.Lock()
	hits = e.g.certHits
	e.g.mu.Unlock()
	if hits != 3 {
		t.Fatalf("certs fetches = %d, want 3 (one unknown-kid refetch, one after max-age)", hits)
	}
}

func TestPushCertsUnavailable(t *testing.T) {
	e := pushEnv(t)
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/oauth2/v3/certs" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 1001)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestPushMethodAndConfig(t *testing.T) {
	e := pushEnv(t)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gmail", nil)
	w := httptest.NewRecorder()
	e.inst.Handler(&testSink{}).ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", w.Code)
	}
	plain := newEnv(t, Config{})
	if w := plain.push(t, plain.token(t, nil), pushBody(testMailbox, 1001)); w.Code != http.StatusNotFound {
		t.Fatalf("push without topic = %d", w.Code)
	}
	caps, _ := plain.inst.Capabilities(t.Context())
	for _, c := range caps {
		if c.Name == model.CapabilityReceiveHTTP && c.Available {
			t.Fatal("receive_http available without a topic")
		}
	}
}
