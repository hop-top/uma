package microsoft

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sendCall is one request the fake Graph saw.
type sendCall struct {
	method, path, auth, contentType string
	body                            []byte
}

// sendAnswer scripts one answer of the fake Graph. The zero value is the
// documented success: 202 with no body.
type sendAnswer struct {
	status     int
	retryAfter string
	drop       bool // the connection drops: no answer
}

// fakeSendMail answers POST .../sendMail as Microsoft Learn documents it:
// 202 Accepted with an empty body, no message ID. Answers are scripted in
// order; after the script every call is 202. A failure body echoes the
// request body and the bearer token, which the adapter must never repeat.
type fakeSendMail struct {
	t   testing.TB
	srv *httptest.Server

	mu     sync.Mutex
	calls  []sendCall
	script []sendAnswer
}

func newFakeSendMail(t testing.TB, script ...sendAnswer) *fakeSendMail {
	f := &fakeSendMail{t: t, script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSendMail) endpoint() string { return f.srv.URL + "/v1.0" }

func (f *fakeSendMail) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, sendCall{
		method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"), body: b,
	})
	ans := sendAnswer{status: http.StatusAccepted}
	if len(f.script) > 0 {
		ans, f.script = f.script[0], f.script[1:]
		if ans.status == 0 {
			ans.status = http.StatusAccepted
		}
	}
	f.mu.Unlock()
	if ans.drop {
		panic(http.ErrAbortHandler)
	}
	if ans.retryAfter != "" {
		w.Header().Set("Retry-After", ans.retryAfter)
	}
	if ans.status/100 == 2 {
		w.WriteHeader(ans.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ans.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "Fake", "message": string(b) + " " + r.Header.Get("Authorization")}})
}

func (f *fakeSendMail) seen() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.calls...)
}

// sendMailBody is the documented sendMail request body, decoded loosely so
// a test sees exactly what went on the wire.
type sendMailBody struct {
	Message         map[string]any `json:"message"`
	SaveToSentItems *bool          `json:"saveToSentItems"`
}

func decodeSendMail(t testing.TB, c sendCall) sendMailBody {
	t.Helper()
	var b sendMailBody
	if err := json.Unmarshal(c.body, &b); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, c.body)
	}
	return b
}

// wireAddrs lists the addresses of a Graph recipient array field.
func wireAddrs(t testing.TB, m map[string]any, field string) []string {
	t.Helper()
	raw, _ := m[field].([]any)
	var out []string
	for _, r := range raw {
		ea, _ := r.(map[string]any)["emailAddress"].(map[string]any)
		a, _ := ea["address"].(string)
		out = append(out, a)
	}
	return out
}

func mustNotContain(t testing.TB, what, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks %q: %s", what, s, text)
		}
	}
}
