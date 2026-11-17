package imap_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// reply is one scripted relay answer for a transaction.
type reply struct {
	// mailErr answers MAIL.
	mailErr *gosmtp.SMTPError
	// applied closes the connection after storing the message, before the
	// final reply (a lost answer); lost closes it without storing (a lost
	// request).
	applied, lost bool
}

type relayTx struct {
	from    string
	rcpts   []string
	data    []byte
	applied bool
}

// relay is an in-process submission server: STARTTLS, AUTH PLAIN for
// mailUser/mailPass, XOAUTH2 and OAUTHBEARER for tokens tokenOK accepts.
// saved, when set, files every applied message (a provider that saves sent
// copies).
type relay struct {
	t    *testing.T
	port int

	mu       sync.Mutex
	script   []reply
	txs      []relayTx
	authOK   bool
	anyCred  bool
	auths    []string
	tokenOK  func(string) bool
	saved    func([]byte)
	authsTLS []bool
}

func newRelay(t *testing.T) *relay {
	t.Helper()
	r := &relay{t: t, authOK: true}
	srv := gosmtp.NewServer(r)
	srv.Domain = "relay.test"
	srv.TLSConfig = serverTLS(t)
	srv.ErrorLog = discardLog{}
	srv.ReadTimeout, srv.WriteTimeout = 10*time.Second, 10*time.Second
	ln := listen(t)
	r.port = port(ln)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return r
}

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

func (r *relay) authList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func (r *relay) setAuth(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authOK = ok
}

func (r *relay) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &relaySession{r: r, c: c, tx: -1}, nil
}

type relaySession struct {
	r      *relay
	c      *gosmtp.Conn
	authed bool
	tx     int
	rep    reply
}

func (s *relaySession) AuthMechanisms() []string {
	return []string{sasl.Plain, "XOAUTH2", sasl.OAuthBearer}
}

func (s *relaySession) accept(mech, user, secret string, bearer bool) error {
	r := s.r
	r.mu.Lock()
	defer r.mu.Unlock()
	_, onTLS := s.c.TLSConnectionState()
	r.auths = append(r.auths, mech)
	r.authsTLS = append(r.authsTLS, onTLS)
	ok := r.authOK && (r.anyCred ||
		!bearer && user == mailUser && secret == mailPass ||
		bearer && user == mailUser && r.tokenOK != nil && r.tokenOK(secret))
	if !ok {
		return gosmtp.ErrAuthFailed
	}
	s.authed = true
	return nil
}

func (s *relaySession) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(_, user, pass string) error { return s.accept(mech, user, pass, false) }), nil
	case sasl.OAuthBearer:
		return refusing{sasl.NewOAuthBearerServer(func(o sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			if s.accept(mech, o.Username, o.Token, true) != nil {
				return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
			}
			return nil
		})}, nil
	case "XOAUTH2":
		return xoauth2Relay{s: s}, nil
	}
	return nil, gosmtp.ErrAuthUnknownMechanism
}

// refusing answers every SASL failure with 535, as submission servers do
// after a bearer token's error challenge.
type refusing struct{ sasl.Server }

func (r refusing) Next(resp []byte) ([]byte, bool, error) {
	ch, done, err := r.Server.Next(resp)
	if err != nil {
		return nil, true, gosmtp.ErrAuthFailed
	}
	return ch, done, nil
}

type xoauth2Relay struct{ s *relaySession }

func (x xoauth2Relay) Next(resp []byte) ([]byte, bool, error) {
	if resp == nil {
		return []byte{}, false, nil
	}
	var user, tok string
	for _, kv := range bytes.Split(resp, []byte{0x01}) {
		k, v, _ := strings.Cut(string(kv), "=")
		switch k {
		case "user":
			user = v
		case "auth":
			tok = strings.TrimPrefix(v, "Bearer ")
		}
	}
	return nil, true, x.s.accept("XOAUTH2", user, tok, true)
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
	if s.rep.mailErr != nil {
		return s.rep.mailErr
	}
	return nil
}

func (s *relaySession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.r.mu.Lock()
	s.r.txs[s.tx].rcpts = append(s.r.txs[s.tx].rcpts, to)
	s.r.mu.Unlock()
	return nil
}

func (s *relaySession) Data(rd io.Reader) error {
	data, err := io.ReadAll(rd)
	if err != nil {
		return err
	}
	if s.rep.lost {
		_ = s.c.Close()
		return errors.New("lost")
	}
	s.r.mu.Lock()
	s.r.txs[s.tx].data, s.r.txs[s.tx].applied = data, true
	saved := s.r.saved
	s.r.mu.Unlock()
	if saved != nil {
		saved(data)
	}
	if s.rep.applied {
		_ = s.c.Close()
		return errors.New("answer lost")
	}
	return nil
}

func (s *relaySession) Reset()        {}
func (s *relaySession) Logout() error { return nil }
