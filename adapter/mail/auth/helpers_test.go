package auth_test

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"

	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// host is a fake ports.Credentials that records every request.
type host struct {
	mu      sync.Mutex
	reqs    []ports.CredentialRequest
	resolve func(ports.CredentialRequest) (ports.Credential, error)
}

func (h *host) Resolve(_ context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	h.mu.Unlock()
	return h.resolve(req)
}

func (h *host) requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.CredentialRequest(nil), h.reqs...)
}

func staticHost(c ports.Credential) *host {
	return &host{resolve: func(ports.CredentialRequest) (ports.Credential, error) { return c, nil }}
}

// rotatingHost returns first, or fresh when the request sets Fresh.
func rotatingHost(first, fresh ports.Credential) *host {
	return &host{resolve: func(r ports.CredentialRequest) (ports.Credential, error) {
		if r.Fresh {
			return fresh, nil
		}
		return first, nil
	}}
}

const (
	user     = "alice@example.com"
	password = "app-password"
)

var expiry = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

func oauth(tok string) ports.OAuth2 {
	return ports.OAuth2{User: user, AccessToken: tok, ExpiresAt: expiry}
}

func pw() ports.Password { return ports.Password{User: user, Secret: []byte(password)} }

var mailKinds = []ports.CredentialKind{ports.KindOAuth2, ports.KindPassword, ports.KindClientCert}

func config(purpose string, d auth.Dialer, h ports.Credentials) auth.Config {
	return auth.Config{
		Credentials: h,
		Request: ports.CredentialRequest{
			Channel:   model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-000000000001"},
			Reference: "cred-ref-1",
			Purpose:   purpose,
			Accept:    mailKinds,
		},
		ServerName: "localhost",
		Dialer:     d,
	}
}

// fakeConn is an auth.Conn with scripted capabilities and exchange.
type fakeConn struct {
	mechs  []string
	state  *tls.ConnectionState
	addr   net.Addr
	auth   func(sasl.Client) error
	closed bool
}

func (f *fakeConn) Mechanisms() []string { return f.mechs }
func (f *fakeConn) RemoteAddr() net.Addr { return f.addr }
func (f *fakeConn) Close() error         { f.closed = true; return nil }

func (f *fakeConn) TLS() (tls.ConnectionState, bool) {
	if f.state == nil {
		return tls.ConnectionState{}, false
	}
	return *f.state, true
}

func (f *fakeConn) Authenticate(c sasl.Client) error {
	if f.auth == nil {
		_, _, err := c.Start()
		return err
	}
	return f.auth(c)
}

var loopback = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1143}

// tlsState returns the client side of a real TLS 1.3 handshake, so
// tls-exporter channel binding works.
func tlsState(t *testing.T) *tls.ConnectionState {
	t.Helper()
	p := testCerts(t)
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	srvCfg := serverTLS(t, p.server)
	srv := tls.Server(b, srvCfg)
	done := make(chan error, 1)
	go func() { done <- srv.Handshake() }()
	cl := tls.Client(a, &tls.Config{ServerName: "localhost", RootCAs: p.roots, MinVersion: tls.VersionTLS13})
	if err := cl.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cs := cl.ConnectionState()
	return &cs
}

// assertSafe fails when an error message carries a secret.
func assertSafe(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range append(secrets, password, "good-token", "stale-token", "fresh-token") {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error %q leaks %q", err, s)
		}
	}
}

func wantCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	if !model.IsCode(err, code) {
		t.Fatalf("err = %v, want code %s", err, code)
	}
	assertSafe(t, err)
}
