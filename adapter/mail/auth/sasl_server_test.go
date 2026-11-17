package auth_test

import (
	"bytes"
	"crypto/tls"
	"errors"
	"strings"
	"sync"

	"github.com/emersion/go-sasl"
	"github.com/xdg-go/scram"
)

// account is the one mailbox every test server knows.
type account struct {
	user     string
	password string
	authzOK  map[string]bool // authorization identities user may act as
	mu       sync.Mutex
	tokens   map[string]bool // valid bearer tokens
	certName string          // client certificate subject accepted by EXTERNAL
}

func newAccount() *account {
	return &account{
		user:     "alice@example.com",
		password: "app-password",
		authzOK:  map[string]bool{"": true, "alice@example.com": true, "shared@example.com": true},
		tokens:   map[string]bool{"good-token": true, "fresh-token": true},
		certName: "alice@example.com",
	}
}

func (a *account) tokenOK(tok string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens[tok]
}

// mechSpec is what a server advertises and how it verifies SCRAM.
type mechSpec struct {
	mechs []string
	// loginDisabled makes the IMAP server advertise LOGINDISABLED and refuse
	// the LOGIN command (Microsoft 365).
	loginDisabled bool
}

func (s mechSpec) advertises(mech string) bool {
	for _, m := range s.mechs {
		if strings.EqualFold(m, mech) {
			return true
		}
	}
	return false
}

// attemptLog records every AUTH a server sees, in order.
type attemptLog struct {
	mu    sync.Mutex
	mechs []string
}

func (l *attemptLog) add(m string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mechs = append(l.mechs, m)
}

func (l *attemptLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.mechs...)
}

var errReject = errors.New("rejected")

// newSASLServer returns the server side of mech. Every failure returns
// errReject; the protocol layer maps it to its own refusal reply. state is
// the connection's TLS state, nil on plaintext.
func newSASLServer(mech string, spec mechSpec, acct *account, state *tls.ConnectionState) (sasl.Server, error) {
	switch strings.ToUpper(mech) {
	case "PLAIN":
		return sasl.NewPlainServer(func(identity, user, pass string) error {
			if user == acct.user && pass == acct.password && acct.authzOK[identity] {
				return nil
			}
			return errReject
		}), nil
	case "LOGIN":
		return &loginServer{acct: acct}, nil
	case "XOAUTH2":
		return &xoauth2Server{acct: acct}, nil
	case "OAUTHBEARER":
		return sasl.NewOAuthBearerServer(func(o sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			if acct.authzOK[o.Username] && acct.tokenOK(o.Token) {
				return nil
			}
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}), nil
	case "EXTERNAL":
		return sasl.NewExternalServer(func(identity string) error {
			if state == nil || len(state.PeerCertificates) == 0 ||
				state.PeerCertificates[0].Subject.CommonName != acct.certName || !acct.authzOK[identity] {
				return errReject
			}
			return nil
		}), nil
	case "SCRAM-SHA-1", "SCRAM-SHA-256", "SCRAM-SHA-256-PLUS":
		return newSCRAMServer(strings.ToUpper(mech), spec, acct, state)
	case "NTLM":
		return &ntlmServer{verify: func(user, domain string, am []byte) bool {
			return strings.EqualFold(domain, ntlmDomain) && strings.EqualFold(user, ntlmUser) &&
				verifyNTLMv2(am, acct.password) == nil
		}}, nil
	}
	// CRAM-MD5 and anything else: advertised for realism, never honoured.
	return nil, errReject
}

type loginServer struct {
	acct *account
	step int
	user string
}

func (s *loginServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	switch {
	case s.step == 1 && resp == nil:
		return []byte("Username:"), false, nil
	case s.step == 1:
		s.step++ // initial response carried the user name
		s.user = string(resp)
		return []byte("Password:"), false, nil
	case s.step == 2:
		s.user = string(resp)
		return []byte("Password:"), false, nil
	case s.step == 3:
		if s.user == s.acct.user && string(resp) == s.acct.password {
			return nil, true, nil
		}
	}
	return nil, true, errReject
}

// xoauth2Server follows Google's XOAUTH2: on a bad token it sends a JSON
// error challenge, then fails after the client's empty response.
type xoauth2Server struct {
	acct   *account
	failed bool
}

func (s *xoauth2Server) Next(resp []byte) ([]byte, bool, error) {
	if s.failed {
		return nil, true, errReject
	}
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
	if s.acct.authzOK[user] && user != "" && s.acct.tokenOK(tok) {
		return nil, true, nil
	}
	s.failed = true
	return []byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`), false, nil
}

// scramServer drives an xdg-go/scram server conversation and sends the
// server-final message as a challenge, as Dovecot does.
type scramServer struct {
	conv *scram.ServerConversation
	sent bool
}

func newSCRAMServer(mech string, spec mechSpec, acct *account, state *tls.ConnectionState) (sasl.Server, error) {
	hash := scram.SHA256
	if mech == "SCRAM-SHA-1" {
		hash = scram.SHA1
	}
	client, err := hash.NewClient(acct.user, acct.password, "")
	if err != nil {
		return nil, err
	}
	stored, err := client.GetStoredCredentialsWithError(scram.KeyFactors{Salt: "c2FsdHNhbHQ=", Iters: 4096})
	if err != nil {
		return nil, err
	}
	srv, err := hash.NewServer(func(user string) (scram.StoredCredentials, error) {
		if user != acct.user {
			return scram.StoredCredentials{}, errReject
		}
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	plus := mech + "-PLUS"
	if mech == "SCRAM-SHA-256-PLUS" {
		plus = mech
	}
	conv := srv.NewConversation()
	if state != nil && spec.advertises(plus) {
		cb, err := scram.NewTLSExporterBinding(state)
		if err != nil {
			return nil, err
		}
		if mech == plus {
			conv = srv.NewConversationWithChannelBindingRequired(cb)
		} else {
			conv = srv.NewConversationWithChannelBinding(cb)
		}
	}
	return &scramServer{conv: conv}, nil
}

func (s *scramServer) Next(resp []byte) ([]byte, bool, error) {
	if s.sent {
		return nil, true, nil
	}
	if resp == nil {
		return []byte{}, false, nil
	}
	out, err := s.conv.Step(string(resp))
	if err != nil {
		return nil, true, errReject
	}
	if s.conv.Done() {
		if !s.conv.Valid() {
			return nil, true, errReject
		}
		s.sent = true
	}
	return []byte(out), false, nil
}

// rejecting maps every server error to the protocol's refusal reply.
type rejecting struct {
	sasl.Server
	refusal error
}

func (r rejecting) Next(resp []byte) ([]byte, bool, error) {
	ch, done, err := r.Server.Next(resp)
	if err != nil {
		return nil, true, r.refusal
	}
	return ch, done, nil
}
