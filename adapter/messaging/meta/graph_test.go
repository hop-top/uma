package meta_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/meta"
)

func client(srv *httptest.Server) meta.Client {
	return meta.Client{HTTP: srv.Client(), BaseURL: srv.URL, Version: "v24.0"}
}

func TestPostSendsBearerJSONToVersionedPath(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	resp, err := client(srv).Post(t.Context(), "106540352242922/messages", "tok", map[string]string{"a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v24.0/106540352242922/messages" {
		t.Errorf("request %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", got.Header)
	}
	if string(body) != `{"a":"b"}` || string(resp.Body) != `{"ok":true}` {
		t.Errorf("body %s, response %s", body, resp.Body)
	}
}

// The documented error object: message, type, code, error_data.details,
// fbtrace_id.
func TestGraphErrorDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"(#131047) Re-engagement message","type":"OAuthException","code":131047,"error_data":{"messaging_product":"whatsapp","details":"Message failed to send because more than 24 hours have passed since the customer last replied to this number."},"fbtrace_id":"AbCdEf"}}`)
	}))
	defer srv.Close()
	_, err := client(srv).Post(t.Context(), "1/messages", "tok", struct{}{})
	var ge *meta.Error
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *meta.Error", err)
	}
	if !ge.Parsed || ge.Code != 131047 || ge.Status != 400 || ge.Type != "OAuthException" || ge.TraceID != "AbCdEf" || ge.Details == "" {
		t.Fatalf("decoded %+v", ge)
	}
	if s := ge.Error(); s != "meta: graph error 131047 (http 400)" {
		t.Errorf("Error() = %q: must carry no provider text", s)
	}
}

func TestUnparsedErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	defer srv.Close()
	_, err := client(srv).Post(t.Context(), "1/messages", "tok", struct{}{})
	var ge *meta.Error
	if !errors.As(err, &ge) || ge.Parsed || ge.Status != http.StatusBadGateway {
		t.Fatalf("err = %#v", err)
	}
}

func TestTransportErrorsTellSentFromNotSent(t *testing.T) {
	// A closed listener refuses the dial: nothing left the host.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, err = meta.Client{HTTP: http.DefaultClient, BaseURL: "http://" + addr, Version: "v24.0"}.Post(t.Context(), "1/messages", "tok", struct{}{})
	var te *meta.TransportError
	if !errors.As(err, &te) || te.Sent {
		t.Fatalf("refused dial = %#v, want not sent", err)
	}

	// A failed TLS handshake happens before any request byte is written.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached the server despite an untrusted certificate")
	}))
	defer tlsSrv.Close()
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	_, err = meta.Client{HTTP: &http.Client{}, BaseURL: tlsSrv.URL, Version: "v24.0"}.Post(t.Context(), "1/messages", "tok", struct{}{})
	if !errors.As(err, &te) || te.Sent {
		t.Fatalf("failed handshake = %#v, want not sent", err)
	}

	// A connection closed after the request was read: maybe applied.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	_, err = client(srv).Post(t.Context(), "1/messages", "tok", struct{}{})
	if !errors.As(err, &te) || !te.Sent {
		t.Fatalf("dropped answer = %#v, want sent", err)
	}
}

func TestContextDeadlineReturnedAsIs(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := client(srv).Post(ctx, "1/messages", "tok", struct{}{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	cases := map[string]struct {
		h    http.Header
		want time.Duration
	}{
		"none":             {h(), 0},
		"seconds":          {h("Retry-After", "120"), 2 * time.Minute},
		"http date":        {h("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat)), 90 * time.Second},
		"past date":        {h("Retry-After", now.Add(-time.Minute).Format(http.TimeFormat)), 0},
		"garbage":          {h("Retry-After", "soon"), 0},
		"usage header":     {h(meta.UsageHeader, `{"102290129340398":[{"type":"whatsapp_business_management","call_count":100,"total_cputime":10,"total_time":10,"estimated_time_to_regain_access":7}]}`), 7 * time.Minute},
		"usage max":        {h(meta.UsageHeader, `{"1":[{"estimated_time_to_regain_access":2},{"estimated_time_to_regain_access":5}],"2":[{"estimated_time_to_regain_access":3}]}`), 5 * time.Minute},
		"retry-after wins": {h("Retry-After", "30", meta.UsageHeader, `{"1":[{"estimated_time_to_regain_access":9}]}`), 30 * time.Second},
		"usage garbage":    {h(meta.UsageHeader, `not json`), 0},
	}
	for name, c := range cases {
		if got := meta.RetryAfter(c.h, now); got != c.want {
			t.Errorf("%s: RetryAfter = %v, want %v", name, got, c.want)
		}
	}
}

func TestClientRefusesBadSetup(t *testing.T) {
	if _, err := (meta.Client{Version: "v24.0"}).Get(t.Context(), "x", "t"); err == nil {
		t.Error("no HTTP client accepted")
	}
	if _, err := (meta.Client{HTTP: http.DefaultClient, Version: "24"}).Get(t.Context(), "x", "t"); err == nil {
		t.Error("bad version accepted")
	}
}
