package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// conn is one IMAP connection as an auth.LoginConn.
type conn struct {
	c   *imapclient.Client
	raw net.Conn  // the TCP connection
	tls *tls.Conn // nil on plaintext
	// updates is signalled when the server reports new mailbox state
	// (EXISTS, EXPUNGE, a flag change), so an IDLE wait wakes up.
	updates chan struct{}
}

func (c *conn) Mechanisms() []string { return c.c.Caps().AuthMechanisms() }
func (c *conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }
func (c *conn) Close() error         { return c.c.Close() }
func (c *conn) LoginCommand() bool   { return !c.c.Caps().Has(imap.CapLoginDisabled) }

func (c *conn) TLS() (tls.ConnectionState, bool) {
	if c.tls == nil {
		return tls.ConnectionState{}, false
	}
	return c.tls.ConnectionState(), true
}

// Authenticate classifies a NO reply as a credential refusal.
func (c *conn) Authenticate(cl sasl.Client) error { return rejected(c.c.Authenticate(cl)) }

// Login runs the LOGIN command; a NO reply is a credential refusal.
func (c *conn) Login(user, password string) error {
	return rejected(c.c.Login(user, password).Wait())
}

func rejected(err error) error {
	var ie *imap.Error
	if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo {
		return fmt.Errorf("%w: %w", auth.ErrRejected, err)
	}
	return err
}

func (c *conn) signal() {
	select {
	case c.updates <- struct{}{}:
	default:
	}
}

var _ auth.LoginConn = (*conn)(nil)

// session is an authenticated IMAP connection with its command timeout.
type session struct {
	*conn
	auth    *auth.Session
	timeout time.Duration
	stop    func() bool // disarms the context watch
}

// connect resolves the credential for PurposeIMAP and opens an
// authenticated session. The session closes when ctx ends.
func (i *Instance) connect(ctx context.Context) (*session, error) {
	if i.deps.Credentials == nil {
		return nil, i.noCredentials(PurposeIMAP)
	}
	timeout := i.cfg.IMAP.commandTimeout()
	// The dial and authentication exchange run under one deadline: go-imap
	// commands are not context-aware, so the deadline closes the socket.
	dctx, cancel := context.WithTimeout(ctx, 2*timeout)
	defer cancel()
	var dialed atomic.Pointer[conn]
	watch := context.AfterFunc(dctx, func() {
		if c := dialed.Load(); c != nil {
			_ = c.raw.Close()
		}
	})
	d := i.imapDialer(func(c *conn) {
		dialed.Store(c)
		if dctx.Err() != nil {
			_ = c.raw.Close()
		}
	})
	as, err := auth.Connect(dctx, auth.Config{
		Credentials: i.deps.Credentials,
		Request: ports.CredentialRequest{
			Channel: i.ch.Ref, Reference: i.ch.CredentialRef, Purpose: PurposeIMAP, Accept: accept,
		},
		ServerName: i.cfg.IMAP.Host,
		RootCAs:    i.f.opts.RootCAs,
		Policy: auth.Policy{
			AllowPlaintextLoopback: i.cfg.IMAP.AllowPlaintextLoopback,
			ReconnectMargin:        i.cfg.reconnectMargin(),
		},
		Dialer: d,
	})
	if !watch() {
		// The deadline fired: whatever Connect returned, the socket is gone.
		if err == nil {
			_ = as.Conn.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, model.NewError(model.Unavailable, PurposeIMAP+": connection timed out")
	}
	if err != nil {
		return nil, err
	}
	c := as.Conn.(*conn)
	s := &session{conn: c, auth: as, timeout: timeout}
	s.stop = context.AfterFunc(ctx, func() { _ = c.raw.Close() })
	return s, nil
}

// close logs out and closes the session.
func (s *session) close() {
	s.stop()
	_ = s.within(func() error { return s.c.Logout().Wait() })
	_ = s.c.Close()
}

// within runs one command exchange; the connection closes when it takes
// longer than the command timeout.
func (s *session) within(fn func() error) error {
	t := time.AfterFunc(s.timeout, func() { _ = s.raw.Close() })
	defer t.Stop()
	return fn()
}

// imapDialer dials the configured IMAP ports. Every connection is ready for
// AUTH: greeting read and, on the STARTTLS port, TLS established by the
// adapter itself so the TLS state (channel binding) stays reachable.
// dialed is called with each connection before its first exchange.
func (i *Instance) imapDialer(dialed func(*conn)) auth.Dialer {
	s := i.cfg.IMAP
	addr := func(port int) string { return net.JoinHostPort(s.Host, strconv.Itoa(port)) }
	nd := &net.Dialer{Timeout: s.commandTimeout(), KeepAlive: 30 * time.Second}
	var d auth.Dialer
	if s.TLSPort != 0 {
		d.TLS = func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			if cfg.NextProtos == nil {
				cfg.NextProtos = []string{"imap"}
			}
			raw, err := (&tls.Dialer{NetDialer: nd, Config: cfg}).DialContext(ctx, "tcp", addr(s.TLSPort))
			if err != nil {
				return nil, err
			}
			tc := raw.(*tls.Conn)
			return newConn(raw, tc, tc, dialed)
		}
	}
	if s.StartTLSPort != 0 {
		d.StartTLS = func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			raw, err := nd.DialContext(ctx, "tcp", addr(s.StartTLSPort))
			if err != nil {
				return nil, err
			}
			tc, greeting, err := startTLS(ctx, raw, cfg, s.commandTimeout())
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			return newConn(raw, tc, &prefixConn{Conn: tc, r: io.MultiReader(bytes.NewReader(greeting), tc)}, dialed)
		}
	}
	if s.PlainPort != 0 && s.TLSPort == 0 && s.StartTLSPort == 0 {
		d.Plain = func(ctx context.Context) (auth.Conn, error) {
			raw, err := nd.DialContext(ctx, "tcp", addr(s.PlainPort))
			if err != nil {
				return nil, err
			}
			return newConn(raw, nil, raw, dialed)
		}
	}
	return d
}

// newConn starts an IMAP client on transport and reads the greeting. A
// PREAUTH greeting is refused: the session must authenticate with the
// channel's credential.
func newConn(raw net.Conn, tc *tls.Conn, transport net.Conn, dialed func(*conn)) (*conn, error) {
	c := &conn{raw: raw, tls: tc, updates: make(chan struct{}, 1)}
	c.c = imapclient.New(emptyResponseConn{transport}, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Expunge: func(uint32) { c.signal() },
			Mailbox: func(m *imapclient.UnilateralDataMailbox) {
				if m.NumMessages != nil {
					c.signal()
				}
			},
			// A flag change another session made: the receiver follows
			// flags, so it wakes. The data is consumed, not read.
			Fetch: func(m *imapclient.FetchMessageData) {
				_, _ = m.Collect()
				c.signal()
			},
		},
	})
	dialed(c)
	if err := c.c.WaitGreeting(); err != nil {
		_ = c.c.Close()
		return nil, err
	}
	if c.c.State() != imap.ConnStateNotAuthenticated {
		_ = c.c.Close()
		return nil, model.NewError(model.Unsupported, PurposeIMAP+": server greeted without authentication")
	}
	return c, nil
}

// syntheticGreeting stands in for the greeting after the adapter's own
// STARTTLS: go-imap reads a greeting first, and capabilities learned before
// TLS are discarded (RFC 9051 section 6.2.1), so it asks again.
var syntheticGreeting = []byte("* OK TLS established\r\n")

// maxLine bounds a response line read before TLS.
const maxLine = 8 << 10

// startTLS runs the pre-TLS part of the protocol on raw: greeting,
// CAPABILITY when the greeting lists none, STARTTLS, and the TLS handshake.
// It refuses a server that does not advertise STARTTLS, answers with
// anything but OK, or sends data after its OK (a plaintext injection). The
// returned greeting replaces the server's for the IMAP client.
func startTLS(ctx context.Context, raw net.Conn, cfg *tls.Config, timeout time.Duration) (*tls.Conn, []byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	_ = raw.SetDeadline(time.Now().Add(timeout))
	br := bufio.NewReaderSize(raw, 4096)
	greet, err := readLine(br)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case hasPrefixFold(greet, "* PREAUTH"):
		return nil, nil, model.NewError(model.Unsupported, PurposeIMAP+": server greeted without authentication")
	case !hasPrefixFold(greet, "* OK"):
		return nil, nil, errors.New("imap: unexpected greeting")
	}
	caps, listed := greetingCaps(greet)
	if !listed {
		if caps, err = command(raw, br, "u1", "CAPABILITY"); err != nil {
			return nil, nil, err
		}
	}
	if !hasCap(caps, "STARTTLS") {
		return nil, nil, model.NewError(model.Unsupported, PurposeIMAP+": server offers no STARTTLS")
	}
	if _, err := command(raw, br, "u2", "STARTTLS"); err != nil {
		return nil, nil, err
	}
	if br.Buffered() > 0 {
		return nil, nil, model.NewError(model.Unsupported, PurposeIMAP+": data after STARTTLS reply")
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return tc, syntheticGreeting, nil
}

// command sends one tagged command and reads to its tagged reply, returning
// the atoms of any untagged CAPABILITY response. A reply other than OK is an
// error.
func command(w io.Writer, br *bufio.Reader, tag, cmd string) ([]string, error) {
	if _, err := io.WriteString(w, tag+" "+cmd+"\r\n"); err != nil {
		return nil, err
	}
	var caps []string
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if hasPrefixFold(line, "* CAPABILITY ") {
			caps = append(caps, strings.Fields(line[len("* CAPABILITY "):])...)
			continue
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		if !hasPrefixFold(line[len(tag)+1:], "OK") {
			return nil, model.NewError(model.Unavailable, PurposeIMAP+": server refused "+cmd)
		}
		return caps, nil
	}
}

// greetingCaps returns the capabilities of a "* OK [CAPABILITY ...]"
// greeting and whether it lists any.
func greetingCaps(greet string) ([]string, bool) {
	rest := greet[len("* OK"):]
	rest = strings.TrimLeft(rest, " ")
	if !hasPrefixFold(rest, "[CAPABILITY ") {
		return nil, false
	}
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return nil, false
	}
	return strings.Fields(rest[len("[CAPABILITY "):end]), true
}

func hasCap(caps []string, name string) bool {
	for _, c := range caps {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// readLine reads one CRLF-terminated line of at most maxLine bytes, without
// its terminator.
func readLine(br *bufio.Reader) (string, error) {
	var b []byte
	for {
		chunk, more, err := br.ReadLine()
		if err != nil {
			return "", err
		}
		b = append(b, chunk...)
		if len(b) > maxLine {
			return "", errors.New("imap: response line too long")
		}
		if !more {
			return string(b), nil
		}
	}
}

// prefixConn reads r (a replacement greeting, then the connection) and
// writes to the connection.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// emptyResponseConn works around go-imap v2 encoding an empty AUTHENTICATE
// continuation response as "=": RFC 9051 reserves "=" for the initial
// response, and Dovecot rejects it (GSSAPI answers the server's AP-REP with
// an empty response, XOAUTH2 answers an error challenge with one). A client
// line that is exactly "=" can only be such a response: every command line
// starts with a tag.
type emptyResponseConn struct{ net.Conn }

func (c emptyResponseConn) Write(p []byte) (int, error) {
	if string(p) == "=\r\n" {
		if _, err := c.Conn.Write([]byte("\r\n")); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}
