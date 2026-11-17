package widget_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

func TestSessionAssuranceIsExplicit(t *testing.T) {
	r := newRig(t, widget.Options{})
	for name, h := range map[string][]string{
		"no assurance":      {"X-Visitor", "alice", "X-Assurance", "", "X-Site", "support"},
		"unknown assurance": {"X-Visitor", "alice", "X-Assurance", "trusted", "X-Site", "support"},
		"verified, no ID":   {"X-Visitor", "", "X-Assurance", widget.AssuranceVerified, "X-Site", "support"},
		"unlisted site":     {"X-Visitor", "alice", "X-Assurance", widget.AssuranceVerified, "X-Site", "billing"},
		"ambiguous site":    {"X-Visitor", "alice", "X-Assurance", widget.AssuranceVerified, "X-Site", ""},
		"verifier refuses":  {"X-Refuse", "1"},
		"overlong ID":       {"X-Visitor", strings.Repeat("v", 257), "X-Assurance", widget.AssuranceVerified, "X-Site", "support"},
	} {
		if code, _ := r.post("/widget/session", "", nil, h...); code != http.StatusUnauthorized {
			t.Errorf("%s: session = %d, want 401", name, code)
		}
	}
}

func TestSessionNamespacesKeepAnonymousApartFromVerified(t *testing.T) {
	r := newRig(t, widget.Options{})
	user := r.session("alice", widget.AssuranceVerified)
	anon := r.session("alice", widget.AssuranceAnonymous)
	random := r.session("", widget.AssuranceAnonymous)
	if user.Visitor != "alice" || anon.Visitor != "alice" || random.Visitor == "" || random.Visitor == "alice" {
		t.Fatalf("visitors = %q %q %q", user.Visitor, anon.Visitor, random.Visitor)
	}
	for _, s := range []widget.SessionResponse{user, anon, random} {
		if code := r.message(s.Token, "1", "hi"); code != http.StatusAccepted {
			t.Fatalf("message = %d", code)
		}
	}
	got := r.sink.received()
	if len(got) != 3 {
		t.Fatalf("ingress = %d, want 3: equal client IDs of three visitors never collide", len(got))
	}
	want := []struct{ ns, kind, assurance string }{
		{widget.NamespaceUser, widget.KindUser, widget.AssuranceVerified},
		{widget.NamespaceVisitor, widget.KindVisitor, widget.AssuranceAnonymous},
		{widget.NamespaceVisitor, widget.KindVisitor, widget.AssuranceAnonymous},
	}
	for i, in := range got {
		env := in.Envelope
		if env.Sender.Namespace != want[i].ns || env.Sender.Kind != want[i].kind || env.Assurance != want[i].assurance {
			t.Errorf("ingress %d sender %+v assurance %q, want %+v", i, env.Sender, env.Assurance, want[i])
		}
		if env.Sender.Channel != chanA || env.Destination != "support" || env.Kind != model.EnvelopeMessage {
			t.Errorf("ingress %d = %+v", i, env)
		}
	}
	if got[0].Envelope.MessageID == got[1].Envelope.MessageID {
		t.Error("a verified and an anonymous visitor with one ID share a message ID")
	}
}

func TestSessionRenewalKeepsSession(t *testing.T) {
	r := newRig(t, widget.Options{})
	first := r.session("", widget.AssuranceAnonymous)
	again := r.session("", widget.AssuranceAnonymous, "Authorization", "Bearer "+first.Token)
	if again.Visitor != first.Visitor || again.Session != first.Session {
		t.Fatalf("renewal = %+v, want visitor %q session %q", again, first.Visitor, first.Session)
	}
	other := r.session("bob", widget.AssuranceVerified, "Authorization", "Bearer "+first.Token)
	if other.Session == first.Session {
		t.Fatal("a token of another visitor passed its session on")
	}
}

func TestSessionTokenExpires(t *testing.T) {
	r := newRig(t, widget.Options{SessionTTL: time.Minute})
	s := r.session("alice", widget.AssuranceVerified)
	if want := r.clock.Now().Add(time.Minute); !s.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", s.ExpiresAt, want)
	}
	r.clock.Advance(time.Minute)
	if code := r.message(s.Token, "1", "late"); code != http.StatusUnauthorized {
		t.Fatalf("message with an expired token = %d, want 401", code)
	}
	if _, code := r.dial(s.Token, ""); code != http.StatusUnauthorized {
		t.Fatalf("event stream with an expired token = %d, want 401", code)
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d, want none", n)
	}
}

func TestSessionTokenForgeriesRefused(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceAnonymous)
	body, sig, _ := strings.Cut(strings.TrimPrefix(s.Token, "v1."), ".")
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	elevated := strings.Replace(string(payload), `"a":"anonymous"`, `"a":"verified"`, 1)
	for name, token := range map[string]string{
		"missing":         "",
		"unsigned":        "v1." + body,
		"empty signature": "v1." + body + ".",
		"wrong signature": "v1." + body + "." + base64.RawURLEncoding.EncodeToString([]byte("not-a-mac-not-a-mac-not-a-mac-00")),
		"elevated claims": "v1." + base64.RawURLEncoding.EncodeToString([]byte(elevated)) + "." + sig,
		"other version":   "v2." + body + "." + sig,
		"garbage":         "not a token",
	} {
		if code := r.message(token, "1", "hi"); code != http.StatusUnauthorized {
			t.Errorf("%s token: message = %d, want 401", name, code)
		}
		if _, code := r.dial(token, ""); code != http.StatusUnauthorized {
			t.Errorf("%s token: event stream = %d, want 401", name, code)
		}
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d after forged tokens, want none", n)
	}
}

func TestSessionTokenBoundToChannelAndKey(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	// Channel B shares A's key: only the channel claim keeps A's tokens
	// off B.
	r.keys.set(chanB, keyA)
	b := r.open(chanB, widget.Configuration("support"))
	srv := httptest.NewServer(b.(adapter.HTTPReceiver).Handler(r.sink))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/messages",
		strings.NewReader(`{"id":"1","text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Token)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("channel A token on channel B = %d, want 401", res.StatusCode)
	}
	r.keys.set(chanA, []byte("rotated-key-rotated-key-rotated!"))
	if code := r.message(s.Token, "1", "hi"); code != http.StatusUnauthorized {
		t.Fatalf("token after key rotation = %d, want 401", code)
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d, want none", n)
	}
}

func TestSessionKeyRequired(t *testing.T) {
	r := newRig(t, widget.Options{})
	r.keys.set(chanA, []byte("short"))
	if code, _ := r.post("/widget/session", "", nil, "X-Visitor", "a", "X-Assurance", widget.AssuranceVerified, "X-Site", "support"); code != http.StatusServiceUnavailable {
		t.Fatalf("session with a short key = %d, want 503", code)
	}
	r.keys.revoke()
	if code, _ := r.post("/widget/session", "", nil, "X-Visitor", "a", "X-Assurance", widget.AssuranceVerified, "X-Site", "support"); code != http.StatusServiceUnavailable {
		t.Fatalf("session with a revoked key = %d, want 503", code)
	}
}

func TestSessionKeyAtLeastHashLength(t *testing.T) {
	r := newRig(t, widget.Options{})
	for n, want := range map[int]int{31: http.StatusServiceUnavailable, 32: http.StatusOK} {
		r.keys.set(chanA, bytes.Repeat([]byte("k"), n))
		if code, _ := r.post("/widget/session", "", nil, "X-Visitor", "a", "X-Assurance", widget.AssuranceVerified, "X-Site", "support"); code != want {
			t.Errorf("session with a %d-byte key = %d, want %d", n, code, want)
		}
	}
}

func TestSessionRemovedSiteEndsSessions(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified, "X-Site", "sales")
	r.open(chanA, widget.Configuration("support"))
	// The rig's handler still serves the instance opened with both sites;
	// the event stream serves from the latest binding.
	if _, code := r.dial(s.Token, ""); code != http.StatusUnauthorized {
		t.Fatalf("event stream for a removed site = %d, want 401", code)
	}
}
