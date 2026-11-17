package smtp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

var (
	tenant    = model.TenantID("smtp-test")
	channelA  = model.ChannelRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-00000000000a"}
	channelB  = model.ChannelRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-00000000000b"}
	relayUser = "relay-user"
	relayPass = "relay-secret-41f0" // #nosec G101 -- test fixture
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// --- PKI -------------------------------------------------------------------

type pki struct {
	roots  *x509.CertPool
	server tls.Certificate
}

var (
	pkiOnce sync.Once
	testPKI pki
	pkiErr  error
)

// certs returns a throwaway CA and a server certificate for localhost and
// 127.0.0.1.
func certs(t *testing.T) pki {
	t.Helper()
	pkiOnce.Do(func() { testPKI, pkiErr = newPKI() })
	if pkiErr != nil {
		t.Fatal(pkiErr)
	}
	return testPKI
}

func newPKI() (pki, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return pki{}, err
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return pki{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return pki{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return pki{}, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		return pki{}, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return pki{roots: roots, server: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}, nil
}

func serverTLS(t *testing.T) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{certs(t).server}, MinVersion: tls.VersionTLS12}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func port(ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

// --- DNS -------------------------------------------------------------------

// zone is a stub resolver: TXT records and A records by name; unknown names
// do not exist, names in temp fail temporarily.
type zone struct {
	mu   sync.Mutex
	txt  map[string][]string
	a    map[string][]net.IP
	temp map[string]bool
}

func newZone() *zone {
	return &zone{txt: map[string][]string{}, a: map[string][]net.IP{}, temp: map[string]bool{}}
}

func (z *zone) setTXT(name string, recs ...string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.txt[strings.ToLower(strings.TrimSuffix(name, "."))] = recs
}

func (z *zone) failTemp(name string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.temp[strings.ToLower(strings.TrimSuffix(name, "."))] = true
}

func (z *zone) lookup(name string) (string, error) {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	if z.temp[n] {
		return n, &net.DNSError{Err: "timeout", Name: name, IsTemporary: true, IsTimeout: true}
	}
	return n, nil
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (z *zone) LookupTXT(_ context.Context, name string) ([]string, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	n, err := z.lookup(name)
	if err != nil {
		return nil, err
	}
	if recs, ok := z.txt[n]; ok {
		return recs, nil
	}
	return nil, notFound(name)
}

func (z *zone) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, notFound(name)
}

func (z *zone) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	n, err := z.lookup(host)
	if err != nil {
		return nil, err
	}
	var out []net.IPAddr
	for _, ip := range z.a[n] {
		out = append(out, net.IPAddr{IP: ip})
	}
	if len(out) == 0 {
		return nil, notFound(host)
	}
	return out, nil
}

func (z *zone) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, notFound(addr)
}

// --- credentials -----------------------------------------------------------

type credHost struct {
	mu   sync.Mutex
	reqs []ports.CredentialRequest
	cred ports.Credential
	err  error
}

func (h *credHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.err != nil {
		return nil, h.err
	}
	return h.cred, nil
}

func (h *credHost) requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.CredentialRequest(nil), h.reqs...)
}

func passwordHost() *credHost {
	return &credHost{cred: ports.Password{User: relayUser, Secret: []byte(relayPass)}}
}

// --- relay -----------------------------------------------------------------

// reply is one scripted relay answer for a transaction.
type reply struct {
	// at is the command answered with err: "MAIL", "RCPT", "DATA" (the 354
	// step) or "END" (the end of DATA).
	at  string
	err *gosmtp.SMTPError
	// hang keeps the end of DATA unanswered until the client gives up;
	// drop closes the connection instead of answering it.
	hang, drop bool
}

// transaction is one MAIL transaction the relay saw.
type relayTx struct {
	from    string
	rcpts   []string
	data    []byte
	applied bool
}

// relay is an in-process submission server: STARTTLS, AUTH PLAIN for
// relayUser/relayPass, scripted replies.
type relay struct {
	t      *testing.T
	mu     sync.Mutex
	script []reply
	txs    []relayTx
	authOK bool // false refuses every AUTH with 535
	// anyCredential accepts every user and password while authOK.
	anyCredential bool
	auths         int
	srv           *gosmtp.Server
	port          int
}

func newRelay(t *testing.T) *relay { return newRelayTLS(t, true) }

// newRelayTLS starts a relay; withTLS false offers no STARTTLS.
func newRelayTLS(t *testing.T, withTLS bool) *relay {
	t.Helper()
	r := &relay{t: t, authOK: true}
	srv := gosmtp.NewServer(r)
	srv.Domain = "relay.test"
	srv.AllowInsecureAuth = !withTLS
	if withTLS {
		srv.TLSConfig = serverTLS(t)
	}
	srv.ErrorLog = discardLog{}
	srv.ReadTimeout, srv.WriteTimeout = 10*time.Second, 10*time.Second
	ln := listen(t)
	r.srv, r.port = srv, port(ln)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return r
}

type discardLog struct{}

func (discardLog) Printf(string, ...any) {}
func (discardLog) Println(...any)        {}

func (r *relay) respond(rs ...reply) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.script = append(r.script, rs...)
}

func (r *relay) transactions() []relayTx {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]relayTx(nil), r.txs...)
}

func (r *relay) authCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auths
}

func asError(err error, target **model.Error) bool { return errors.As(err, target) }

func (r *relay) setAuth(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authOK = ok
}

func (r *relay) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &relaySession{r: r, c: c}, nil
}

type relaySession struct {
	r      *relay
	c      *gosmtp.Conn
	authed bool
	tx     int // index into r.txs; -1 outside a transaction
	rep    reply
}

func (s *relaySession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *relaySession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		s.r.mu.Lock()
		defer s.r.mu.Unlock()
		s.r.auths++
		if !s.r.authOK || !s.r.anyCredential && (user != relayUser || pass != relayPass) {
			return gosmtp.ErrAuthFailed
		}
		s.authed = true
		return nil
	}), nil
}

func (s *relaySession) update(fn func(*relayTx)) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	fn(&s.r.txs[s.tx])
}

func (s *relaySession) Mail(from string, _ *gosmtp.MailOptions) error {
	if !s.authed {
		return gosmtp.ErrAuthRequired
	}
	s.r.mu.Lock()
	s.rep = reply{}
	if len(s.r.script) > 0 {
		s.rep, s.r.script = s.r.script[0], s.r.script[1:]
	}
	s.r.txs = append(s.r.txs, relayTx{from: from})
	s.tx = len(s.r.txs) - 1
	s.r.mu.Unlock()
	if s.rep.at == "MAIL" {
		return s.rep.err
	}
	return nil
}

func (s *relaySession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.update(func(tx *relayTx) { tx.rcpts = append(tx.rcpts, to) })
	if s.rep.at == "RCPT" {
		return s.rep.err
	}
	return nil
}

func (s *relaySession) Data(r io.Reader) error {
	if s.rep.at == "DATA" {
		_, _ = io.Copy(io.Discard, r)
		return s.rep.err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if s.rep.at == "END" {
		return s.rep.err
	}
	s.update(func(tx *relayTx) { tx.data, tx.applied = data, true })
	switch {
	case s.rep.drop:
		_ = s.c.Close()
		return errors.New("dropped")
	case s.rep.hang:
		time.Sleep(3 * time.Second)
		_ = s.c.Close()
		return errors.New("hung")
	}
	return nil
}

func (s *relaySession) Reset()        {}
func (s *relaySession) Logout() error { return nil }

// --- configuration -----------------------------------------------------------

func config(relayPort int) smtp.Config {
	return smtp.Config{
		Hostname: "mx.example.com",
		Recipients: []smtp.Recipient{
			{Address: "support@example.com", Destination: "support"},
			{Address: "sales@example.com", Destination: "sales"},
		},
		From: "support@example.com", FromName: "Support", Subject: "Your request",
		Relay: smtp.Relay{Host: "127.0.0.1", StartTLSPort: relayPort, CommandTimeoutMS: 2000, DataTimeoutMS: 500},
	}
}

func channel(ref model.ChannelRef, cfg smtp.Config) model.Channel {
	return model.Channel{Ref: ref, Type: model.TypeSmtpMail, Configuration: cfg.Extension(), CredentialRef: "relay-cred", Revision: "ch1"}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var now = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

func open(t *testing.T, f *smtp.Factory, ch model.Channel, creds ports.Credentials) *smtp.Instance {
	t.Helper()
	inst, err := f.Open(t.Context(), ch, adapter.Dependencies{Credentials: creds, Clock: fixedClock{now}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.(*smtp.Instance)
}
