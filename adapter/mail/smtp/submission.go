package smtp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Submission is one submission server and the credential that authenticates
// to it. smtp.mail relays its outbound mail through one; other mail adapters
// (imap.mail) submit through one with their mailbox credential.
type Submission struct {
	Credentials ports.Credentials
	// Request identifies the credential. Its Purpose prefixes every safe
	// message, naming the protocol that failed.
	Request ports.CredentialRequest
	// Server is the submission server: ports, plaintext opt-in, timeouts.
	Server Relay
	// Hostname is the EHLO name.
	Hostname string
	// RootCAs verifies the server certificate; nil uses the system roots.
	RootCAs *x509.CertPool
}

// Submit runs one mail transaction from from to every address of rcpts
// (envelope recipients, To, Cc and Bcc alike) through package auth
// (TLS before AUTH, one credential resolution for the connection). Replies
// are definitive: 2xx after the end of DATA is provider_accepted (Outcome
// without references), a 5xx reply is rejected, a 4xx reply or a transport
// failure before the end of DATA is transient, and a missing reply after it
// is outcome_unknown, never to be resent.
func (s Submission) Submit(ctx context.Context, from string, rcpts []string, body []byte) (adapter.Outcome, error) {
	purpose := s.Request.Purpose
	if s.Credentials == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: purpose + ": credentials port not configured", Retry: model.NoRetry}
	}
	sess, err := auth.Connect(ctx, auth.Config{
		Credentials: s.Credentials,
		Request:     s.Request,
		ServerName:  s.Server.Host,
		RootCAs:     s.RootCAs,
		Policy:      auth.Policy{AllowPlaintextLoopback: s.Server.AllowPlaintextLoopback},
		Dialer:      s.dialer(),
	})
	if err != nil {
		return adapter.Outcome{}, err
	}
	conn := sess.Conn.(*relayConn)
	defer func() { _ = conn.c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.raw.Close() })
	defer stop()
	if err := submit(conn.c, from, rcpts, body); err != nil {
		return submissionOutcome(purpose, err)
	}
	_ = conn.c.Quit()
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// submission stages, for error mapping.
type stage string

const (
	stageMail stage = "MAIL"
	stageRcpt stage = "RCPT"
	stageData stage = "DATA"
	stageBody stage = "body"
	stageEnd  stage = "end of DATA"
)

// submitError is a failed submission step. After stageEnd the server may
// have accepted the message.
type submitError struct {
	stage stage
	err   error
}

func (e *submitError) Error() string { return string(e.stage) + ": " + e.err.Error() }
func (e *submitError) Unwrap() error { return e.err }

// submit runs one mail transaction. A recipient the server refuses ends
// the transaction: nothing is delivered to the others.
func submit(c *gosmtp.Client, from string, rcpts []string, body []byte) error {
	if err := c.Mail(from, nil); err != nil {
		return &submitError{stageMail, err}
	}
	for _, to := range rcpts {
		if err := c.Rcpt(to, nil); err != nil {
			return &submitError{stageRcpt, err}
		}
	}
	w, err := c.Data()
	if err != nil {
		return &submitError{stageData, err}
	}
	if _, err := w.Write(body); err != nil {
		return &submitError{stageBody, err}
	}
	// Close sends the terminating dot and reads the final reply: from here
	// on the server may hold the message.
	if _, err := w.CloseWithResponse(); err != nil {
		return &submitError{stageEnd, err}
	}
	return nil
}

// submissionOutcome maps a failed submission: a 5xx reply is rejected, a 4xx
// reply or a transport failure before the end of DATA is transient, and a
// missing final reply is outcome_unknown.
func submissionOutcome(purpose string, err error) (adapter.Outcome, error) {
	var se *submitError
	if !errors.As(err, &se) {
		return adapter.Outcome{}, model.NewError(model.Transient, purpose+": submission failed")
	}
	var reply *gosmtp.SMTPError
	if errors.As(se.err, &reply) {
		desc := purpose + ": server answered " + replyCode(reply) + " to " + string(se.stage)
		switch {
		case reply.Code >= 500 && reply.Code < 600:
			return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: model.Permanent, SafeMessage: desc, Retry: model.NoRetry}}, nil
		case reply.Code >= 400 && reply.Code < 500:
			return adapter.Outcome{}, model.NewError(model.Transient, desc)
		}
	}
	if se.stage == stageEnd {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, purpose+": no reply to the end of DATA")
	}
	return adapter.Outcome{}, model.NewError(model.Transient, purpose+": connection failed during "+string(se.stage))
}

// replyCode renders a reply's numeric codes only; reply text is provider
// data and never enters a safe message.
func replyCode(e *gosmtp.SMTPError) string {
	s := strconv.Itoa(e.Code)
	if ec := e.EnhancedCode; ec != gosmtp.NoEnhancedCode && ec != (gosmtp.EnhancedCode{}) {
		s += fmt.Sprintf(" %d.%d.%d", ec[0], ec[1], ec[2])
	}
	return s
}

var msgIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// MessageID derives the Message-ID of an operation's outbound mail, "<…@domain>":
// the same for every attempt, distinct across operations and tenants,
// revealing neither.
func MessageID(op model.OperationRef, domain string) string {
	sum := sha256.Sum256([]byte(string(op.Tenant) + "\x00" + string(op.ID)))
	return "<" + strings.ToLower(msgIDEncoding.EncodeToString(sum[:20])) + "@" + domain + ">"
}

// relayConn is a go-smtp client as an auth.Conn.
type relayConn struct {
	c   *gosmtp.Client
	raw net.Conn
}

func (r *relayConn) Mechanisms() []string {
	ok, params := r.c.Extension("AUTH")
	if !ok {
		return nil
	}
	return strings.Fields(params)
}

func (r *relayConn) TLS() (tls.ConnectionState, bool) { return r.c.TLSConnectionState() }
func (r *relayConn) RemoteAddr() net.Addr             { return r.raw.RemoteAddr() }
func (r *relayConn) Close() error                     { return r.c.Close() }

// Authenticate classifies 535 as a credential refusal.
func (r *relayConn) Authenticate(cl sasl.Client) error {
	err := r.c.Auth(cl)
	var se *gosmtp.SMTPError
	if errors.As(err, &se) && se.Code == 535 {
		return fmt.Errorf("%w: %w", auth.ErrRejected, err)
	}
	return err
}

// dialer dials the configured server ports. Every connection is ready for
// AUTH: greeting read and EHLO sent (after STARTTLS when used).
func (s Submission) dialer() auth.Dialer {
	r := s.Server
	addr := func(port int) string { return net.JoinHostPort(r.Host, strconv.Itoa(port)) }
	ready := func(raw net.Conn, c *gosmtp.Client) (auth.Conn, error) {
		c.CommandTimeout, c.SubmissionTimeout = r.commandTimeout(), r.dataTimeout()
		if err := c.Hello(s.Hostname); err != nil {
			_ = c.Close()
			return nil, err
		}
		return &relayConn{c: c, raw: raw}, nil
	}
	var d auth.Dialer
	if r.TLSPort != 0 {
		d.TLS = func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			raw, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr(r.TLSPort))
			if err != nil {
				return nil, err
			}
			return ready(raw, gosmtp.NewClient(raw))
		}
	}
	if r.StartTLSPort != 0 {
		d.StartTLS = func(ctx context.Context, cfg *tls.Config) (auth.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr(r.StartTLSPort))
			if err != nil {
				return nil, err
			}
			stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
			defer stop()
			c, err := gosmtp.NewClientStartTLS(raw, cfg)
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			// go-smtp handshakes on first use; EHLO completes it.
			return ready(raw, c)
		}
	}
	if r.PlainPort != 0 && r.TLSPort == 0 && r.StartTLSPort == 0 {
		d.Plain = func(ctx context.Context) (auth.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr(r.PlainPort))
			if err != nil {
				return nil, err
			}
			return ready(raw, gosmtp.NewClient(raw))
		}
	}
	return d
}
