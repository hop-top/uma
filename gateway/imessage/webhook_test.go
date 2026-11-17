package imessage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
)

// umaStandIn verifies what the gateway posts the way the relay channel's
// auth configuration does (scheme hmac, X-Webhook-Signature, a signed Unix
// timestamp in X-Webhook-Timestamp) and keeps each verified body.
type umaStandIn struct {
	srv    *httptest.Server
	status int

	mu     sync.Mutex
	bodies []map[string]any
	refuse int
}

func newUmaStandIn(t *testing.T) *umaStandIn {
	t.Helper()
	v, err := webhookauth.New(webhookauth.Config{
		Scheme: webhookauth.SchemeHMAC, Header: SignatureHeader,
		Timestamp: webhookauth.Timestamp{Header: TimestampHeader},
	}, webhookauth.SecretFunc(func(context.Context) ([][]byte, error) { return [][]byte{[]byte(webhookSecret)}, nil }), webhookauth.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	u := &umaStandIn{status: http.StatusOK}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _, err := v.VerifyHTTP(r)
		u.mu.Lock()
		defer u.mu.Unlock()
		if err != nil {
			u.refuse++
			v.Reject(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		u.bodies = append(u.bodies, m)
		w.WriteHeader(u.status)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *umaStandIn) received() []map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]map[string]any(nil), u.bodies...)
}

func webhookRig(t *testing.T, be *scriptedBackend, u *umaStandIn, mutate ...func(*Config)) *rig {
	t.Helper()
	return newRig(t, be, append([]func(*Config){func(c *Config) { c.WebhookURL = u.srv.URL + "/uma" }}, mutate...)...)
}

func (r *rig) postWebhook(path, body string, header ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

var inbound = Inbound{
	From: "alice@example.com", To: "+15550100002", Body: "hello", MessageID: "m-1",
	Media: []Media{{URL: "https://cdn.example.com/a.jpg", ContentType: "image/jpeg"}},
}

// What the gateway posts verifies under the relay's hmac configuration and
// carries the relay's neutral field names.
func TestWebhookForwardsSignedRelayJSON(t *testing.T) {
	u := newUmaStandIn(t)
	var forwarded []string
	r := webhookRig(t, &scriptedBackend{msgs: []Inbound{inbound}}, u, func(c *Config) {
		c.Forwarded = func(_ []byte, from string) { forwarded = append(forwarded, from) }
	})
	if rec := r.postWebhook(PathWebhook+"/path-token-0123456789", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body)
	}
	got := u.received()
	if len(got) != 1 {
		t.Fatalf("uma received %d messages (refused %d)", len(got), u.refuse)
	}
	m := got[0]
	if m["from"] != "alice@example.com" || m["to"] != "+15550100002" || m["body"] != "hello" || m["message_id"] != "m-1" {
		t.Errorf("forwarded %v", m)
	}
	media, _ := m["media"].([]any)
	if len(media) != 1 || media[0].(map[string]any)["url"] != "https://cdn.example.com/a.jpg" || media[0].(map[string]any)["content_type"] != "image/jpeg" {
		t.Errorf("media %v", m["media"])
	}
	if len(forwarded) != 1 || forwarded[0] != "alice@example.com" {
		t.Errorf("Forwarded hook calls %v", forwarded)
	}
}

// A signature under another secret is refused by uma: the gateway signs with
// the secret it is configured with, not with anything uma would take.
func TestWebhookSignatureIsTheConfiguredSecret(t *testing.T) {
	u := newUmaStandIn(t)
	r := webhookRig(t, &scriptedBackend{msgs: []Inbound{inbound}}, u, func(c *Config) {
		c.WebhookSecret = []byte("another-secret-0123456789")
	})
	if rec := r.postWebhook(PathWebhook+"/path-token-0123456789", `{}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("answer %d, want 502: uma refused the message", rec.Code)
	}
	if u.refuse != 1 || len(u.received()) != 0 {
		t.Fatalf("uma refused %d, received %d", u.refuse, len(u.received()))
	}
}

// A backend that signs nothing is reached only through the secret path
// token; a wrong token is 404, before anything is parsed or forwarded.
func TestWebhookPathToken(t *testing.T) {
	u := newUmaStandIn(t)
	r := webhookRig(t, &scriptedBackend{msgs: []Inbound{inbound}}, u)
	for _, path := range []string{PathWebhook, PathWebhook + "/wrong-token-0123456789", PathWebhook + "/path-token-012345678"} {
		if rec := r.postWebhook(path, `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, rec.Code)
		}
	}
	if n := len(u.received()); n != 0 {
		t.Fatalf("uma received %d messages", n)
	}
}

// A backend that signs: its verification runs before anything is forwarded.
func TestWebhookBackendSignature(t *testing.T) {
	u := newUmaStandIn(t)
	r := webhookRig(t, &scriptedBackend{signs: true, msgs: []Inbound{inbound}}, u, func(c *Config) { c.WebhookToken = "" })
	if rec := r.postWebhook(PathWebhook, `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned answered %d, want 401", rec.Code)
	}
	if n := len(u.received()); n != 0 {
		t.Fatalf("uma received %d messages from an unsigned webhook", n)
	}
	if rec := r.postWebhook(PathWebhook, `{}`, "X-Test-Signature", "ok"); rec.Code != http.StatusOK {
		t.Errorf("signed answered %d, want 200", rec.Code)
	}
	if n := len(u.received()); n != 1 {
		t.Fatalf("uma received %d messages, want 1", n)
	}
}

// When uma does not take a message the backend is answered 502, so a backend
// that retries delivers it again; an event carrying nothing is answered 200.
func TestWebhookAnswers(t *testing.T) {
	u := newUmaStandIn(t)
	u.status = http.StatusServiceUnavailable
	r := webhookRig(t, &scriptedBackend{msgs: []Inbound{inbound}}, u)
	if rec := r.postWebhook(PathWebhook+"/path-token-0123456789", `{}`); rec.Code != http.StatusBadGateway {
		t.Errorf("uma 503 answered %d, want 502", rec.Code)
	}
	r = webhookRig(t, &scriptedBackend{}, u)
	if rec := r.postWebhook(PathWebhook+"/path-token-0123456789", `{}`); rec.Code != http.StatusOK {
		t.Errorf("nothing to forward answered %d, want 200", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, PathWebhook+"/path-token-0123456789", nil)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d, want 405", rec.Code)
	}
}
