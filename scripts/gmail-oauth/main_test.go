package main

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoogle is a token endpoint that checks the client credentials and,
// for an authorization code, the PKCE verifier against the challenge the
// consent URL carried.
type fakeGoogle struct {
	mu        sync.Mutex
	challenge string
	forms     []url.Values
	// release, when set, holds the answer to an authorization code until it
	// is closed, so a test can finish reading the loopback page first.
	release chan struct{}
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.forms = append(f.forms, r.PostForm)
	challenge := f.challenge
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != "csecret" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if f.release != nil {
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3599}`)
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != "rt-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at-2","expires_in":3599}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func newClient(t *testing.T, f *fakeGoogle) client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return client{id: "cid", secret: "csecret", tokenURL: srv.URL, http: srv.Client()}
}

// syncWriter collects what consent prints while the test reads it.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestConsentLoopbackFlow(t *testing.T) {
	// Consent closes its loopback server once the code is exchanged, which
	// would cut a reply still being written to the browser. Hold the exchange
	// until the callback's answer is read.
	f := &fakeGoogle{release: make(chan struct{})}
	c := newClient(t, f)
	var stderr syncWriter
	type result struct {
		tok token
		err error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := c.consent(t.Context(), defaultScopes, "mailbox@example.test", &stderr)
		done <- result{tok, err}
	}()

	var consentURL *url.URL
	for deadline := time.Now().Add(5 * time.Second); consentURL == nil; {
		if time.Now().After(deadline) {
			t.Fatalf("no consent URL printed: %q", stderr.String())
		}
		for _, line := range strings.Split(stderr.String(), "\n") {
			if strings.HasPrefix(line, authURL+"?") {
				u, err := url.Parse(line)
				if err != nil {
					t.Fatal(err)
				}
				consentURL = u
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	q := consentURL.Query()
	for k, want := range map[string]string{
		"client_id": "cid", "scope": defaultScopes, "access_type": "offline", "prompt": "consent",
		"response_type": "code", "code_challenge_method": "S256", "login_hint": "mailbox@example.test",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("consent URL %s = %q, want %q", k, got, want)
		}
	}
	redirect := q.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Fatalf("redirect_uri %q is not a loopback address", redirect)
	}
	f.mu.Lock()
	f.challenge = q.Get("code_challenge")
	f.mu.Unlock()

	// The browser lands on the loopback redirect with the code.
	resp, err := http.Get(redirect + "/?" + url.Values{"code": {"the-code"}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	close(f.release)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
	r := <-done
	if r.err != nil || r.tok.RefreshToken != "rt-1" || r.tok.AccessToken != "at-1" {
		t.Fatalf("consent = %+v, %v", r.tok, r.err)
	}
	if got := f.forms[0].Get("redirect_uri"); got != redirect {
		t.Errorf("exchange redirect_uri %q, want %q", got, redirect)
	}
}

func TestCallbackRejectsWrongState(t *testing.T) {
	for _, q := range []url.Values{
		{"code": {"c"}, "state": {"other"}},
		{"error": {"access_denied"}, "state": {"s"}},
		{"state": {"s"}},
	} {
		if _, err := callback(q, "s"); err == nil {
			t.Errorf("callback(%v) accepted", q)
		}
	}
	if code, err := callback(url.Values{"code": {"c"}, "state": {"s"}}, "s"); err != nil || code != "c" {
		t.Fatalf("callback = %q, %v", code, err)
	}
}

func TestRefreshExchange(t *testing.T) {
	c := newClient(t, &fakeGoogle{})
	tok, err := c.exchange(t.Context(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"rt-1"}})
	if err != nil || tok.AccessToken != "at-2" {
		t.Fatalf("exchange = %+v, %v", tok, err)
	}
	_, err = c.exchange(t.Context(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"stale"}})
	if err == nil || !strings.Contains(err.Error(), "7 days") {
		t.Fatalf("expired refresh token error = %v, want the testing-mode hint", err)
	}
}

func TestRunNeedsClient(t *testing.T) {
	t.Setenv(envClientID, "")
	t.Setenv(envClientSecret, "")
	var out, errOut strings.Builder
	if err := run(t.Context(), []string{"token"}, &out, &errOut); err == nil || out.Len() != 0 {
		t.Fatalf("run without client = %v, stdout %q", err, out.String())
	}
}
