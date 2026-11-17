package unsubscribetest

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
)

// Target is the stand-in for a sender's one-click endpoint (RFC 8058,
// https://www.rfc-editor.org/rfc/rfc8058): a local TLS server that records
// what it is asked and answers with the scripted status; a 3xx status
// redirects to /followed, which a test fails on reaching. A recorded
// exchange with a real sender replaces it.
type Target struct {
	srv *httptest.Server

	mu     sync.Mutex
	hits   []string
	bodies []string
	status int
}

// NewTarget starts a Target answering 200.
func NewTarget(t testing.TB) *Target {
	t.Helper()
	tg := &Target{status: http.StatusOK}
	tg.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 128)
		n, _ := r.Body.Read(buf)
		tg.mu.Lock()
		tg.hits = append(tg.hits, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Content-Type")+" cookie="+r.Header.Get("Cookie")+" auth="+r.Header.Get("Authorization"))
		tg.bodies = append(tg.bodies, string(buf[:n]))
		status := tg.status
		tg.mu.Unlock()
		if r.URL.Path == "/followed" {
			t.Error("the redirect was followed")
		}
		if status/100 == 3 {
			http.Redirect(w, r, "/followed", status)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

// SetStatus scripts the answer.
func (tg *Target) SetStatus(status int) {
	tg.mu.Lock()
	tg.status = status
	tg.mu.Unlock()
}

// Requests are the requests seen: "METHOD target content-type cookie=
// auth=".
func (tg *Target) Requests() []string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]string(nil), tg.hits...)
}

// Bodies are the request bodies seen, in order.
func (tg *Target) Bodies() []string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]string(nil), tg.bodies...)
}

// URI is an https URI of the Target, on the host name example.com (the one
// its certificate names) and the Target's port.
func (tg *Target) URI(path string) string {
	_, port, _ := net.SplitHostPort(tg.srv.Listener.Addr().String())
	return "https://example.com:" + port + path
}

// Resolve answers every host name with one public address, as the stand-in's
// resolver: the connect-time guard still sees a public address, and Dial then
// reaches the Target.
func (tg *Target) Resolve(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

// Dial connects to the Target whatever address it is asked for.
func (tg *Target) Dial(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, tg.srv.Listener.Addr().String())
}

// RootCAs trusts the Target's certificate.
func (tg *Target) RootCAs() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(tg.srv.Certificate())
	return pool
}
