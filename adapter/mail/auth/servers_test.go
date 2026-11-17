package auth_test

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// security is how a test server listens.
type security int

const (
	implicitTLS security = iota // TLS from the first byte
	startTLS                    // plaintext, STARTTLS offered
	plaintext                   // no TLS at all; AUTH allowed anyway
)

// testServer is one in-process IMAP or SMTP server on 127.0.0.1.
type testServer struct {
	addr     string
	attempts *attemptLog
	acct     *account
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// --- SMTP -----------------------------------------------------------------

type smtpBackend struct {
	spec     mechSpec
	acct     *account
	attempts *attemptLog
}

func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{b: b, c: c}, nil
}

type smtpSession struct {
	b *smtpBackend
	c *smtp.Conn
}

func (s *smtpSession) AuthMechanisms() []string { return s.b.spec.mechs }

func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	s.b.attempts.add(mech)
	if !s.b.spec.advertises(mech) {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	var state *tls.ConnectionState
	if cs, ok := s.c.TLSConnectionState(); ok {
		state = &cs
	}
	srv, err := newSASLServer(mech, s.b.spec, s.b.acct, state)
	if err != nil {
		return nil, smtp.ErrAuthFailed
	}
	return rejecting{Server: srv, refusal: smtp.ErrAuthFailed}, nil
}

func (s *smtpSession) Mail(string, *smtp.MailOptions) error { return nil }
func (s *smtpSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *smtpSession) Data(r io.Reader) error               { _, err := io.Copy(io.Discard, r); return err }
func (s *smtpSession) Reset()                               {}
func (s *smtpSession) Logout() error                        { return nil }

func newSMTPServer(t *testing.T, spec mechSpec, sec security) *testServer {
	t.Helper()
	p := testCerts(t)
	ts := &testServer{attempts: &attemptLog{}, acct: newAccount()}
	srv := smtp.NewServer(&smtpBackend{spec: spec, acct: ts.acct, attempts: ts.attempts})
	srv.Domain = "localhost"
	ln := listen(t)
	switch sec {
	case implicitTLS:
		ln = tls.NewListener(ln, serverTLS(t, p.server))
	case startTLS:
		srv.TLSConfig = serverTLS(t, p.server)
	case plaintext:
		srv.AllowInsecureAuth = true
	}
	ts.addr = ln.Addr().String()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ts
}

// --- IMAP -----------------------------------------------------------------

type imapSession struct {
	imapserver.Session
	conn     *imapserver.Conn
	spec     mechSpec
	acct     *account
	attempts *attemptLog
}

func (s *imapSession) AuthenticateMechanisms() []string { return s.spec.mechs }

func (s *imapSession) Authenticate(mech string) (sasl.Server, error) {
	s.attempts.add(mech)
	if !s.spec.advertises(mech) {
		return nil, imapserver.ErrAuthFailed
	}
	var state *tls.ConnectionState
	if c, ok := s.conn.NetConn().(interface{ ConnectionState() tls.ConnectionState }); ok {
		cs := c.ConnectionState()
		state = &cs
	}
	srv, err := newSASLServer(mech, s.spec, s.acct, state)
	if err != nil {
		return nil, imapserver.ErrAuthFailed
	}
	return rejecting{Server: loggedIn{Server: srv, s: s}, refusal: imapserver.ErrAuthFailed}, nil
}

func (s *imapSession) Login(user, pass string) error {
	s.attempts.add("IMAP-LOGIN")
	if s.spec.loginDisabled {
		return imapserver.ErrAuthFailed
	}
	return s.Session.Login(user, pass)
}

// loggedIn moves the memory session to the authenticated state once SASL
// succeeds.
type loggedIn struct {
	sasl.Server
	s *imapSession
}

func (l loggedIn) Next(resp []byte) ([]byte, bool, error) {
	ch, done, err := l.Server.Next(resp)
	if done && err == nil {
		err = l.s.Session.Login(l.s.acct.user, l.s.acct.password)
	}
	return ch, done, err
}

func newIMAPServer(t *testing.T, spec mechSpec, sec security) *testServer {
	t.Helper()
	p := testCerts(t)
	ts := &testServer{attempts: &attemptLog{}, acct: newAccount()}
	mem := imapmemserver.New()
	mem.AddUser(imapmemserver.NewUser(ts.acct.user, ts.acct.password))
	opts := &imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &imapSession{Session: mem.NewSession(), conn: c, spec: spec, acct: ts.acct, attempts: ts.attempts}, nil, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}},
	}
	ln := listen(t)
	switch sec {
	case implicitTLS:
		ln = tls.NewListener(ln, serverTLS(t, p.server))
	case startTLS:
		opts.TLSConfig = serverTLS(t, p.server)
	case plaintext:
		opts.InsecureAuth = true
	}
	if spec.loginDisabled {
		// imapserver only advertises LOGINDISABLED on connections it cannot
		// authenticate; inject it on a TLS connection as Microsoft 365 does.
		opts.InsecureAuth = true
		ln = capRewriter{ln}
	}
	ts.addr = ln.Addr().String()
	srv := imapserver.New(opts)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ts
}

// capRewriter adds LOGINDISABLED to every capability list the server writes.
type capRewriter struct{ net.Listener }

func (l capRewriter) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return rewriteConn{c}, nil
}

type rewriteConn struct{ net.Conn }

var (
	capFrom = []byte("IMAP4rev1 SASL-IR")
	capTo   = []byte("IMAP4rev1 LOGINDISABLED SASL-IR")
)

func (c rewriteConn) Write(b []byte) (int, error) {
	if !bytes.Contains(b, capFrom) {
		return c.Conn.Write(b)
	}
	if _, err := c.Conn.Write(bytes.ReplaceAll(b, capFrom, capTo)); err != nil {
		return 0, err
	}
	return len(b), nil
}
