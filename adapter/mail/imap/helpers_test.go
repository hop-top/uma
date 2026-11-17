package imap_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

var (
	tenant   = model.TenantID("imap-test")
	channelA = model.ChannelRef{Tenant: tenant, ID: "00000e00-0000-7000-8000-00000000000a"}
)

const (
	mailUser = "desk@example.com"
	mailPass = "app-password-5c1e" // #nosec G101 -- test fixture
	peer     = "alice@example.org"
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// --- PKI -------------------------------------------------------------------

type pki struct {
	roots  *x509.CertPool
	server tls.Certificate
	client tls.Certificate // CN mailUser, for EXTERNAL
}

var (
	pkiOnce sync.Once
	testPKI pki
	pkiErr  error
)

// certs returns a throwaway CA, a server certificate for localhost and
// 127.0.0.1 and a client certificate.
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
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imap test CA"},
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
	leaf := func(serial int64, cn string, server bool) (tls.Certificate, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if server {
			tmpl.DNSNames, tmpl.IPAddresses = []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)}
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			return tls.Certificate{}, err
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: c}, nil
	}
	srv, err := leaf(2, "localhost", true)
	if err != nil {
		return pki{}, err
	}
	cli, err := leaf(3, mailUser, false)
	if err != nil {
		return pki{}, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return pki{roots: roots, server: srv, client: cli}, nil
}

func serverTLS(t *testing.T) *tls.Config {
	p := certs(t)
	return &tls.Config{
		Certificates: []tls.Certificate{p.server}, MinVersion: tls.VersionTLS12,
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: p.roots,
	}
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

// --- credentials -------------------------------------------------------------

// credHost is the host credential port: it records every request and
// answers with resolve.
type credHost struct {
	mu      sync.Mutex
	reqs    []ports.CredentialRequest
	resolve func(ports.CredentialRequest, int) (ports.Credential, error)
}

func (h *credHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	h.reqs = append(h.reqs, r)
	n := len(h.reqs)
	h.mu.Unlock()
	return h.resolve(r, n)
}

func (h *credHost) requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.CredentialRequest(nil), h.reqs...)
}

func staticCreds(c ports.Credential) *credHost {
	return &credHost{resolve: func(ports.CredentialRequest, int) (ports.Credential, error) { return c, nil }}
}

func passwordCreds() *credHost {
	return staticCreds(ports.Password{User: mailUser, Secret: []byte(mailPass)})
}

// --- sink ----------------------------------------------------------------

// sink records every ingress handed to it; fail, while set, refuses them.
type sink struct {
	mu   sync.Mutex
	got  []model.Ingress
	fail error
	seen chan struct{}
	// dedupe answers a message envelope whose event ID it holds already as
	// a duplicate, as the engine does, and records nothing.
	dedupe bool
}

func newSink() *sink { return &sink{seen: make(chan struct{}, 1024)} }

func (s *sink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	if in.Envelope.Kind != model.EnvelopeConversationState {
		if err := in.Envelope.Content.Validate(); err != nil {
			return model.IngressReceipt{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return model.IngressReceipt{}, s.fail
	}
	if s.dedupe && in.Envelope.Kind != model.EnvelopeConversationState {
		for _, g := range s.got {
			if g.Envelope.EventID == in.Envelope.EventID {
				return model.IngressReceipt{Duplicate: true}, nil
			}
		}
	}
	s.got = append(s.got, in)
	s.seen <- struct{}{}
	return model.IngressReceipt{}, nil
}

func (s *sink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "no interactions")
}

func (s *sink) envelopes() []model.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Envelope, len(s.got))
	for i, in := range s.got {
		out[i] = in.Envelope
	}
	return out
}

func (s *sink) setFail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// await waits until the sink holds n envelopes and returns them.
func (s *sink) await(t *testing.T, n int) []model.Envelope {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if got := s.envelopes(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink holds %d envelopes, want %d", len(s.envelopes()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// messageIDs returns the Message-IDs of the envelopes, in order.
func messageIDs(envs []model.Envelope) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.MessageID
	}
	return out
}

// observer records observation kinds.
type observer struct {
	mu    sync.Mutex
	kinds []string
}

func (o *observer) Observe(_ context.Context, ob ports.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.kinds = append(o.kinds, ob.Kind)
}

func (o *observer) count(kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, k := range o.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// protocolState is an in-memory adapter.ProtocolState.
type protocolState struct {
	mu   sync.Mutex
	rev  int
	data map[string]model.Extension
}

func (p *protocolState) Load(_ context.Context, key string) (model.Extension, model.Revision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	x, ok := p.data[key]
	if !ok {
		return model.Extension{}, "", model.NewError(model.NotFound, "no state")
	}
	return x, model.Revision(fmt.Sprint(p.rev)), nil
}

func (p *protocolState) CompareAndSwap(_ context.Context, key string, rev model.Revision, x model.Extension) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := model.Revision("")
	if _, ok := p.data[key]; ok {
		cur = model.Revision(fmt.Sprint(p.rev))
	}
	if cur != rev {
		return model.NewError(model.Conflict, "revision mismatch")
	}
	if p.data == nil {
		p.data = map[string]model.Extension{}
	}
	p.rev++
	p.data[key] = x
	return nil
}

// --- configuration -----------------------------------------------------------

// config is a channel configuration for srv (IMAP) and relay (submission),
// both on 127.0.0.1.
func config(srv *mailServer, relay *relay) imap.Config {
	c := imap.Config{
		Address: mailUser, FromName: "Desk", Subject: "Your request",
		IMAP:           srv.endpoint(),
		IdleRestartMS:  60_000,
		PollIntervalMS: 50,
	}
	if relay != nil {
		c.Submission = imap.Server{Host: "127.0.0.1", StartTLSPort: relay.port, CommandTimeoutMS: 5000, DataTimeoutMS: 1000}
	} else {
		c.Submission = imap.Server{Host: "127.0.0.1", TLSPort: 1}
	}
	return c
}

func channel(ref model.ChannelRef, cfg imap.Config) model.Channel {
	return model.Channel{Ref: ref, Type: model.TypeImapMail, Configuration: cfg.Extension(), CredentialRef: "mailbox-cred", Revision: "ch1"}
}

func newFactory(t *testing.T) *imap.Factory {
	return imap.NewFactory(imap.Options{RootCAs: certs(t).roots, ReconnectBackoff: 20 * time.Millisecond})
}

// steppedFactory returns a factory whose receiver waits only as the test
// steps w.
func steppedFactory(t *testing.T, w *stepWaiter) *imap.Factory {
	return imap.NewFactory(imap.Options{RootCAs: certs(t).roots, Waiter: w, Jitter: func() float64 { return 0.5 }})
}

// stepWaiter is a ports.Waiter the test drives: a wait returns only when
// released, never with time. Releasing a wait advances clock, when set, by
// the waited duration.
type stepWaiter struct {
	clock *portstest.FixedClock

	mu        sync.Mutex
	pending   []chan struct{}
	durations []time.Duration
	waits     []time.Duration // durations of pending waits, by position
}

func (w *stepWaiter) Wait(ctx context.Context, d time.Duration) error {
	ch := make(chan struct{})
	w.mu.Lock()
	w.pending = append(w.pending, ch)
	w.waits = append(w.waits, d)
	w.durations = append(w.durations, d)
	w.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		w.mu.Lock()
		for i, p := range w.pending {
			if p == ch {
				w.pending = append(w.pending[:i], w.pending[i+1:]...)
				w.waits = append(w.waits[:i], w.waits[i+1:]...)
				break
			}
		}
		w.mu.Unlock()
		return ctx.Err()
	}
}

// waiting reports whether a wait is pending: the receiver finished its round
// and nothing else will happen until a release or a mailbox change.
func (w *stepWaiter) waiting() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) > 0
}

// idle waits until the receiver is waiting.
func (w *stepWaiter) idle(t *testing.T) {
	t.Helper()
	waitFor(t, w.waiting)
}

// release ends every pending wait, advancing the clock by the longest.
func (w *stepWaiter) release() {
	w.mu.Lock()
	pending, waits := w.pending, w.waits
	w.pending, w.waits = nil, nil
	w.mu.Unlock()
	if w.clock != nil && len(waits) > 0 {
		w.clock.Advance(slices.Max(waits))
	}
	for _, ch := range pending {
		close(ch)
	}
}

// step waits for the receiver to wait, then releases it.
func (w *stepWaiter) step(t *testing.T) {
	t.Helper()
	w.idle(t)
	w.release()
}

// drive releases every wait as it comes until cond holds.
func (w *stepWaiter) drive(t *testing.T, cond func() bool) {
	t.Helper()
	waitFor(t, func() bool {
		if cond() {
			return true
		}
		w.release()
		return false
	})
}

func (w *stepWaiter) requested() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.durations...)
}

func open(t *testing.T, f *imap.Factory, ch model.Channel, deps adapter.Dependencies) *imap.Instance {
	t.Helper()
	inst, err := f.Open(t.Context(), ch, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.(*imap.Instance)
}

// running is one Run in the background.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func run(t *testing.T, inst *imap.Instance, s adapter.Sink) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- inst.Run(ctx, s) }()
	t.Cleanup(func() { _ = r.stop() })
	return r
}

// stop cancels the run and returns its result.
func (r *running) stop() error {
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(10 * time.Second):
		return errors.New("Run did not stop")
	}
}

// result waits for Run to return on its own.
func (r *running) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

// --- messages ----------------------------------------------------------------

// mail is a raw inbound message from peer with Message-ID id.
func mail(id, text string) []byte {
	return []byte(crlf("From: Alice <" + peer + ">\nTo: " + mailUser + "\nSubject: hello\n" +
		"Date: Sun, 11 Oct 2026 09:00:00 +0000\nMessage-ID: " + id + "\n" +
		"Content-Type: text/plain; charset=utf-8\n\n" + text + "\n"))
}
