package xrrtest_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	xrr "hop.top/xrr"

	"hop.top/uma/testing/xrrtest"
)

// spyTB captures failures so miss and mode errors can be asserted without
// failing the enclosing test.
type spyTB struct {
	testing.TB
	mu     sync.Mutex
	errors []string
	fatals []string
}

func (s *spyTB) Errorf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, fmt.Sprintf(format, args...))
}

func (s *spyTB) Fatalf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fatals = append(s.fatals, fmt.Sprintf(format, args...))
}

func get(t *testing.T, c *http.Client, rawURL string, hdr http.Header) (*http.Response, string) {
	t.Helper()
	return do(t, c, http.MethodGet, rawURL, "", "", hdr)
}

func do(t *testing.T, c *http.Client, method, rawURL, contentType, body string, hdr http.Header) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, r)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

// cassettes returns the concatenated contents of every file in dir.
func cassettes(t *testing.T, dir string) (string, []string) {
	t.Helper()
	fsys := os.DirFS(dir)
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	var names []string
	for _, e := range entries {
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Name())
		all.Write(b)
	}
	return all.String(), names
}

// offline is a transport that fails the test if replay reaches it.
func offline(t *testing.T) xrrtest.Option {
	return xrrtest.WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("replay reached the network: %s %s", r.Method, r.URL)
		return nil, errors.New("offline")
	}))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecordReplayRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/messages/42")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"42","echo":%q}`, string(b))
	}))
	defer srv.Close()
	dir := t.TempDir()

	rec := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeRecord))
	if got := rec.Transport.(*xrrtest.Transport).Mode(); got != xrrtest.ModeRecord {
		t.Fatalf("mode = %q", got)
	}
	r1, b1 := do(t, rec, http.MethodPost, srv.URL+"/messages", "text/plain", "hello", nil)

	rep := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))
	r2, b2 := do(t, rep, http.MethodPost, srv.URL+"/messages", "text/plain", "hello", nil)

	if r1.StatusCode != http.StatusCreated || r2.StatusCode != http.StatusCreated {
		t.Fatalf("status record=%d replay=%d", r1.StatusCode, r2.StatusCode)
	}
	if b1 != b2 || b2 != `{"id":"42","echo":"hello"}` {
		t.Fatalf("body record=%q replay=%q", b1, b2)
	}
	for _, h := range []string{"Content-Type", "Location"} {
		if r1.Header.Get(h) != r2.Header.Get(h) || r2.Header.Get(h) == "" {
			t.Errorf("%s record=%q replay=%q", h, r1.Header.Get(h), r2.Header.Get(h))
		}
	}
	if r2.ContentLength != int64(len(b2)) || r2.Request == nil {
		t.Errorf("replayed response: ContentLength=%d Request=%v", r2.ContentLength, r2.Request)
	}
}

func TestHeaderAllowList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Ratelimit-Remaining", "3")
		w.Header().Set("X-Custom", "kept")
		w.Header().Set("X-Request-Id", "volatile-123")
		w.Header().Set("Set-Cookie", "session=s3cr3t")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	dir := t.TempDir()
	opts := []xrrtest.Option{xrrtest.WithHeaders("X-Custom", "Authorization", "Set-Cookie", "X-Api-*")}

	hdr := http.Header{
		"Authorization": {"Bearer abcdefghijklmnopqrstuvwxyz"},
		"X-Api-Key":     {"key-123"},
		"X-Trace":       {"trace-1"},
	}
	rec := xrrtest.NewClient(t, dir, append(opts, xrrtest.WithMode(xrrtest.ModeRecord))...)
	get(t, rec, srv.URL+"/limited", hdr)

	all, _ := cassettes(t, dir)
	for _, want := range []string{"Retry-After", "X-Ratelimit-Remaining", "X-Custom", "Content-Type"} {
		if !strings.Contains(all, want) {
			t.Errorf("cassette lacks allowed header %s:\n%s", want, all)
		}
	}
	for _, bad := range []string{"Authorization", "Bearer", "X-Api-Key", "key-123", "X-Trace", "X-Request-Id", "Set-Cookie", "s3cr3t"} {
		if strings.Contains(all, bad) {
			t.Errorf("cassette contains %q:\n%s", bad, all)
		}
	}

	rep := xrrtest.NewClient(t, dir, append(opts, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))...)
	r, _ := get(t, rep, srv.URL+"/limited", hdr)
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") != "7" ||
		r.Header.Get("X-RateLimit-Remaining") != "3" || r.Header.Get("X-Custom") != "kept" {
		t.Errorf("replay status=%d headers=%v", r.StatusCode, r.Header)
	}
	if r.Header.Get("X-Request-Id") != "" || r.Header.Get("Set-Cookie") != "" {
		t.Errorf("replay leaked unlisted headers: %v", r.Header)
	}
}

func TestScrubSecrets(t *testing.T) {
	const ghToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"access_token":"real-access","id":"msg_1","count":2,"note":"pat %s","items":["%s","plain"],"internal":"corp-only"}`, ghToken, ghToken)
	}))
	defer srv.Close()
	dir := t.TempDir()
	custom := xrrtest.WithScrub(func(_ string, b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "corp-only", "SCRUBBED"))
	})

	form := "grant_type=client_credentials&client_secret=real-secret&client_id=app"
	rec := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeRecord), custom)
	_, recBody := do(t, rec, http.MethodPost, srv.URL+"/oauth/token", "application/x-www-form-urlencoded", form, nil)

	all, _ := cassettes(t, dir)
	for _, bad := range []string{"real-access", "real-secret", ghToken, "corp-only", "<redacted:BODY>"} {
		if strings.Contains(all, bad) {
			t.Errorf("cassette contains %q:\n%s", bad, all)
		}
	}
	for _, want := range []string{"msg_1", "<redacted:ACCESS_TOKEN>", "client_secret=%3Credacted%3ACLIENT_SECRET%3E", "<redacted:NOTE>", "<redacted:ITEMS>", "plain", "SCRUBBED", "client_id=app"} {
		if !strings.Contains(all, want) {
			t.Errorf("cassette lacks %q:\n%s", want, all)
		}
	}

	// A different secret in replay still matches: request bodies are
	// scrubbed before fingerprinting.
	fake := "grant_type=client_credentials&client_secret=fake&client_id=app"
	rep := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), custom, offline(t))
	_, repBody := do(t, rep, http.MethodPost, srv.URL+"/oauth/token", "application/x-www-form-urlencoded", fake, nil)
	if recBody != repBody {
		t.Errorf("record and replay bodies differ:\nrecord %s\nreplay %s", recBody, repBody)
	}
}

func TestScrubSecretsUnchanged(t *testing.T) {
	for _, tc := range []struct{ ct, body string }{
		{"application/json", `{"id": "1",  "n": 1.50}`},
		{"application/x-www-form-urlencoded", "b=2&a=1"},
		{"text/plain", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"application/json", `not json`},
	} {
		if got := string(xrrtest.ScrubSecrets(tc.ct, []byte(tc.body))); got != tc.body {
			t.Errorf("ScrubSecrets(%q, %q) = %q, want unchanged", tc.ct, tc.body, got)
		}
	}
	got := string(xrrtest.ScrubSecrets("application/vnd.api+json", []byte(`{"n":12345678901234567890,"password":"x"}`)))
	if got != `{"n":12345678901234567890,"password":"<redacted:PASSWORD>"}` {
		t.Errorf("ScrubSecrets +json = %s", got)
	}
}

func TestNormalize(t *testing.T) {
	const realAccount, fakeAccount = "AC0123456789real", "ACfake"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acct := strings.Split(r.URL.Path, "/")[2]
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/Accounts/"+acct+"/Messages/1")
		_ = json.NewEncoder(w).Encode(map[string]string{"account_sid": acct})
	}))
	defer srv.Close()
	dir := t.TempDir()
	tagged := xrrtest.WithNormalize(func(r *xrrtest.Request) {
		r.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
	})
	opts := func(mode xrrtest.Mode, acct string) []xrrtest.Option {
		return []xrrtest.Option{
			xrrtest.WithMode(mode), xrrtest.WithDropQuery("ts"), tagged,
			xrrtest.WithPlaceholder(acct, "ACCOUNT_SID"),
		}
	}

	rec := xrrtest.NewClient(t, dir, opts(xrrtest.ModeRecord, realAccount)...)
	do(t, rec, http.MethodPost, srv.URL+"/Accounts/"+realAccount+"/Messages?b=2&a=1&ts=111&access_token=tok",
		"application/x-www-form-urlencoded", "To=%2B1555&Body=hi", nil)

	all, _ := cassettes(t, dir)
	if strings.Contains(all, realAccount) || strings.Contains(all, "ts=111") || strings.Contains(all, "access_token=tok") {
		t.Errorf("cassette not normalized:\n%s", all)
	}
	for _, want := range []string{"/Accounts/ACCOUNT_SID/Messages?a=1&access_token=%3Credacted%3AACCESS_TOKEN%3E&b=2", "Body=hi&To=%2B1555", `"account_sid":"ACCOUNT_SID"`, "Location: /Accounts/ACCOUNT_SID/Messages/1"} {
		if !strings.Contains(all, want) {
			t.Errorf("cassette lacks %q:\n%s", want, all)
		}
	}

	// Different query order, timestamp, token, form order, trailing slash
	// and account all normalize to the recorded request.
	rep := xrrtest.NewClient(t, dir, append(opts(xrrtest.ModeReplay, fakeAccount), offline(t))...)
	r, body := do(t, rep, http.MethodPost, srv.URL+"/Accounts/"+fakeAccount+"/Messages/?access_token=other&ts=999&a=1&b=2",
		"application/x-www-form-urlencoded", "Body=hi&To=%2B1555", nil)
	if body != "{\"account_sid\":\"ACfake\"}\n" || r.Header.Get("Location") != "/Accounts/ACfake/Messages/1" {
		t.Errorf("replay did not restore placeholder: body=%s location=%s", body, r.Header.Get("Location"))
	}
}

func TestHostInFingerprint(t *testing.T) {
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, name) })
	}
	a := httptest.NewServer(handler("a"))
	defer a.Close()
	b := httptest.NewServer(handler("b"))
	defer b.Close()
	dir := t.TempDir()

	rec := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeRecord))
	get(t, rec, a.URL+"/same", nil)
	get(t, rec, b.URL+"/same", nil)
	if _, names := cassettes(t, dir); len(names) != 4 {
		t.Fatalf("want 2 cassettes (4 files) for 2 hosts, got %v", names)
	}

	rep := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))
	if _, body := get(t, rep, b.URL+"/same", nil); body != "b" {
		t.Errorf("host b replayed %q", body)
	}
	if _, body := get(t, rep, a.URL+"/same", nil); body != "a" {
		t.Errorf("host a replayed %q", body)
	}
}

func TestReplayMiss(t *testing.T) {
	dir := t.TempDir()
	spy := &spyTB{TB: t}
	c := xrrtest.NewClient(spy, dir, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.test/v1/things?id=7", nil)
	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, xrr.ErrCassetteMiss) {
		t.Fatalf("err = %v, want ErrCassetteMiss", err)
	}
	if len(spy.errors) != 1 {
		t.Fatalf("want one test failure, got %q", spy.errors)
	}
	msg := spy.errors[0]
	for _, want := range []string{"GET", "https://api.example.test/v1/things?id=7", "fingerprint ", filepath.Join(dir, "http-"), ".resp.yaml", "XRR_MODE=record"} {
		if !strings.Contains(msg, want) || !strings.Contains(err.Error(), strings.TrimSuffix(want, "XRR_MODE=record")) {
			t.Errorf("miss message lacks %q:\n%s\nerr: %v", want, msg, err)
		}
	}
}

func TestRepeatedCalls(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "call %d", n.Add(1))
	}))
	defer srv.Close()
	dir := t.TempDir()

	rec := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeRecord))
	for range 3 {
		get(t, rec, srv.URL+"/poll", nil)
	}
	if _, names := cassettes(t, dir); len(names) != 6 {
		t.Fatalf("want 3 cassettes (6 files), got %v", names)
	}

	// Occurrence numbering restarts per transport, so each replay client
	// sees the same sequence.
	for range 2 {
		rep := xrrtest.NewClient(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))
		for i := 1; i <= 3; i++ {
			if _, body := get(t, rep, srv.URL+"/poll", nil); body != fmt.Sprintf("call %d", i) {
				t.Errorf("replay %d = %q", i, body)
			}
		}
		spy := &spyTB{TB: t}
		extra := xrrtest.NewTransport(spy, dir, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))
		for range 4 {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/poll", nil)
			if resp, err := extra.RoundTrip(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		if len(spy.errors) != 1 {
			t.Errorf("fourth identical call: want one miss, got %q", spy.errors)
		}
	}
}

func TestRecordedTransportError(t *testing.T) {
	dir := t.TempDir()
	failing := xrrtest.WithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}))
	target := "https://api.example.test/down"

	rec := xrrtest.NewTransport(t, dir, xrrtest.WithMode(xrrtest.ModeRecord), failing)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if _, err := rec.RoundTrip(req); err == nil || err.Error() != "dial tcp: connection refused" {
		t.Fatalf("record err = %v", err)
	}

	rep := xrrtest.NewTransport(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), offline(t))
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	_, err := rep.RoundTrip(req)
	if err == nil || err.Error() != "dial tcp: connection refused" {
		t.Fatalf("replay err = %v, want recorded message", err)
	}
	if errors.Is(err, xrr.ErrCassetteMiss) {
		t.Fatal("recorded error replayed as a miss")
	}
}

func TestModeFromEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cassettes")

	t.Setenv(xrrtest.EnvMode, "")
	if m := xrrtest.NewTransport(t, dir).Mode(); m != xrrtest.ModeReplay {
		t.Errorf("unset mode = %q, want replay", m)
	}

	t.Setenv(xrrtest.EnvMode, "passthrough")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "live") }))
	defer srv.Close()
	pt := xrrtest.NewClient(t, dir)
	if _, body := get(t, pt, srv.URL, nil); body != "live" {
		t.Errorf("passthrough body = %q", body)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("passthrough touched the cassette dir: %v", err)
	}

	t.Setenv(xrrtest.EnvMode, "record")
	if m := xrrtest.NewTransport(t, dir).Mode(); m != xrrtest.ModeRecord {
		t.Errorf("mode = %q, want record", m)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("record mode did not create %s: %v", dir, err)
	}

	t.Setenv(xrrtest.EnvMode, "rewind")
	spy := &spyTB{TB: t}
	if tr := xrrtest.NewTransport(spy, dir); tr != nil || len(spy.fatals) != 1 || !strings.Contains(spy.fatals[0], `"rewind"`) {
		t.Errorf("invalid mode: transport=%v fatals=%q", tr, spy.fatals)
	}
}

func TestRequestNotMutated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("ts") != "1" || string(b) != "b=2&a=1" {
			t.Errorf("provider saw normalized request: %s %q", r.URL, b)
		}
	}))
	defer srv.Close()
	c := xrrtest.NewClient(t, t.TempDir(), xrrtest.WithMode(xrrtest.ModeRecord), xrrtest.WithDropQuery("ts"))
	u, _ := url.Parse(srv.URL + "/x?ts=1")
	do(t, c, http.MethodPost, u.String(), "application/x-www-form-urlencoded", "b=2&a=1", nil)
}
