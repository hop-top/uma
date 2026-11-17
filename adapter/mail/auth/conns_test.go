package auth_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"hop.top/uma/adapter/mail/auth"
)

// The wrappers below are what the IMAP and SMTP adapters do: map a protocol
// client onto auth.Conn and classify refusals as auth.ErrRejected.

type smtpConn struct {
	c   *smtp.Client
	raw net.Conn
}

func (s *smtpConn) Mechanisms() []string {
	ok, params := s.c.Extension("AUTH")
	if !ok {
		return nil
	}
	return strings.Fields(params)
}

func (s *smtpConn) TLS() (tls.ConnectionState, bool) { return s.c.TLSConnectionState() }
func (s *smtpConn) RemoteAddr() net.Addr             { return s.raw.RemoteAddr() }
func (s *smtpConn) Close() error                     { return s.c.Close() }

func (s *smtpConn) Authenticate(cl sasl.Client) error {
	err := s.c.Auth(cl)
	var se *smtp.SMTPError
	if errors.As(err, &se) && se.Code == 535 {
		return fmt.Errorf("%w: %w", auth.ErrRejected, err)
	}
	return err
}

func smtpDialer(addr string) auth.Dialer {
	return auth.Dialer{
		TLS: func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			raw, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			return &smtpConn{c: smtp.NewClient(raw), raw: raw}, nil
		},
		StartTLS: func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			c, err := smtp.NewClientStartTLS(raw, cfg)
			if err != nil {
				return nil, err
			}
			// go-smtp handshakes on first use; EHLO completes it.
			if err := c.Hello("localhost"); err != nil {
				_ = c.Close()
				return nil, err
			}
			return &smtpConn{c: c, raw: raw}, nil
		},
		Plain: func(ctx context.Context) (auth.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			return &smtpConn{c: smtp.NewClient(raw), raw: raw}, nil
		},
	}
}

type imapConn struct {
	c      *imapclient.Client
	tlsRaw *tls.Conn // nil on plaintext
	raw    net.Conn
}

func (i *imapConn) Mechanisms() []string { return i.c.Caps().AuthMechanisms() }
func (i *imapConn) RemoteAddr() net.Addr { return i.raw.RemoteAddr() }
func (i *imapConn) Close() error         { return i.c.Close() }
func (i *imapConn) LoginCommand() bool   { return !i.c.Caps().Has(imap.CapLoginDisabled) }

func (i *imapConn) TLS() (tls.ConnectionState, bool) {
	if i.tlsRaw == nil {
		return tls.ConnectionState{}, false
	}
	return i.tlsRaw.ConnectionState(), true
}

func (i *imapConn) Authenticate(cl sasl.Client) error { return imapRejected(i.c.Authenticate(cl)) }
func (i *imapConn) Login(user, pass string) error {
	return imapRejected(i.c.Login(user, pass).Wait())
}

func imapRejected(err error) error {
	var ie *imap.Error
	if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo {
		return fmt.Errorf("%w: %w", auth.ErrRejected, err)
	}
	return err
}

// imapDialer offers implicit TLS and plaintext. imapclient does not expose
// the TLS state after its own STARTTLS, so STARTTLS is exercised over SMTP.
func imapDialer(addr string) auth.Dialer {
	return auth.Dialer{
		TLS: func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			raw, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			tc := raw.(*tls.Conn)
			c := imapclient.New(emptyResponseConn{tc}, nil)
			if err := c.WaitGreeting(); err != nil {
				_ = c.Close()
				return nil, err
			}
			return &imapConn{c: c, tlsRaw: tc, raw: raw}, nil
		},
		Plain: func(ctx context.Context) (auth.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			c := imapclient.New(raw, nil)
			if err := c.WaitGreeting(); err != nil {
				_ = c.Close()
				return nil, err
			}
			return &imapConn{c: c, raw: raw}, nil
		},
	}
}

// emptyResponseConn works around go-imap v2 encoding an empty AUTHENTICATE
// continuation response as "=": RFC 9051 reserves "=" for the initial
// response, and Dovecot rejects it (GSSAPI answers the AP-REP with an empty
// response). A client line that is exactly "=" can only be such a response:
// every command line starts with a tag.
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

var (
	_ auth.LoginConn = (*imapConn)(nil)
	_ auth.Conn      = (*smtpConn)(nil)
)
