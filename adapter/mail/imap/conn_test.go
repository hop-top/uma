package imap_test

import (
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-sasl"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func goodToken(tok string) bool { return tok == "good-token" }

// TestMechanismsAndTransports: every supported mechanism authenticates the
// receiver over the transports it is used on, always over TLS unless the
// channel opted into plaintext loopback.
func TestMechanismsAndTransports(t *testing.T) {
	password := ports.Password{User: mailUser, Secret: []byte(mailPass)}
	token := ports.OAuth2{User: mailUser, AccessToken: "good-token", ExpiresAt: time.Now().Add(time.Hour)}
	for _, c := range []struct {
		name    string
		sec     security
		mechs   []string
		cred    ports.Credential
		want    string
		wantTLS bool
	}{
		{"implicit TLS PLAIN", implicitTLS, []string{sasl.Plain}, password, sasl.Plain, true},
		{"implicit TLS LOGIN", implicitTLS, []string{sasl.Login}, password, sasl.Login, true},
		{"implicit TLS LOGIN command", implicitTLS, []string{}, password, "IMAP-LOGIN", true},
		{"implicit TLS SCRAM-SHA-256-PLUS", implicitTLS, []string{"SCRAM-SHA-256", "SCRAM-SHA-256-PLUS"}, password, "SCRAM-SHA-256-PLUS", true},
		{"STARTTLS SCRAM-SHA-256-PLUS", startTLS, []string{"SCRAM-SHA-256", "SCRAM-SHA-256-PLUS", sasl.Plain}, password, "SCRAM-SHA-256-PLUS", true},
		{"STARTTLS SCRAM-SHA-256", startTLS, []string{"SCRAM-SHA-256"}, password, "SCRAM-SHA-256", true},
		{"STARTTLS XOAUTH2", startTLS, []string{"XOAUTH2"}, token, "XOAUTH2", true},
		{"STARTTLS OAUTHBEARER", startTLS, []string{"XOAUTH2", sasl.OAuthBearer}, token, sasl.OAuthBearer, true},
		{"implicit TLS EXTERNAL", implicitTLS, []string{sasl.External, sasl.Plain}, ports.ClientCert{Certificate: certs(t).client}, sasl.External, true},
		{"plaintext loopback PLAIN", plaintext, []string{sasl.Plain}, password, sasl.Plain, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newMailServer(t, serverOptions{sec: c.sec, mechs: c.mechs, tokenOK: goodToken})
			inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: staticCreds(c.cred)})
			srv.deliver("INBOX", mail("<m1@example.org>", "hi"), time.Now())
			s := newSink()
			run(t, inst, s)
			s.await(t, 1)
			logins := srv.loginList()
			if len(logins) == 0 || logins[0].mech != c.want || logins[0].tls != c.wantTLS {
				t.Fatalf("logins %+v, want %s with TLS %v", logins, c.want, c.wantTLS)
			}
		})
	}
}

// TestStartTLSNotOffered: a STARTTLS port whose server offers no STARTTLS
// is refused before any AUTH.
func TestStartTLSNotOffered(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: plaintext})
	cfg := config(srv, nil)
	cfg.IMAP.PlainPort, cfg.IMAP.AllowPlaintextLoopback, cfg.IMAP.StartTLSPort = 0, false, srv.port
	inst := open(t, newFactory(t), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds()})
	err := run(t, inst, newSink()).result(t)
	if model.CodeOf(err) != model.Unsupported {
		t.Fatalf("Run = %v, want unsupported", err)
	}
	if a := srv.attemptList(); len(a) != 0 {
		t.Fatalf("server saw authentication attempts %v on a plaintext connection", a)
	}
	if n := srv.commands("LOGIN") + srv.commands("AUTHENTICATE"); n != 0 {
		t.Fatalf("credentials sent %d times without TLS", n)
	}
}

// TestEmptyContinuationResponse: an empty SASL response goes out as an
// empty line, never as "=": SCRAM answers the server-final message with
// one, XOAUTH2 answers the error challenge of a stale token with one before
// the fresh-token retry.
func TestEmptyContinuationResponse(t *testing.T) {
	t.Run("SCRAM", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, mechs: []string{"SCRAM-SHA-256"}})
		inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
		srv.deliver("INBOX", mail("<m1@example.org>", "hi"), time.Now())
		s := newSink()
		run(t, inst, s)
		s.await(t, 1)
		lines := srv.clientLines()
		if slices.Contains(lines, "=") || !slices.Contains(lines, "") {
			t.Fatalf("client lines %q: want an empty response line and no \"=\"", lines)
		}
	})
	t.Run("XOAUTH2 fresh retry", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, mechs: []string{"XOAUTH2"}, tokenOK: goodToken})
		creds := &credHost{resolve: func(r ports.CredentialRequest, _ int) (ports.Credential, error) {
			tok := "stale-token"
			if r.Fresh {
				tok = "good-token"
			}
			return ports.OAuth2{User: mailUser, AccessToken: tok}, nil
		}}
		inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: creds})
		srv.deliver("INBOX", mail("<m1@example.org>", "hi"), time.Now())
		s := newSink()
		run(t, inst, s)
		s.await(t, 1)
		lines := srv.clientLines()
		if slices.Contains(lines, "=") || !slices.Contains(lines, "") {
			t.Fatalf("client lines %q: want an empty response line and no \"=\"", lines)
		}
		if l := srv.loginList(); len(l) != 1 || l[0].token != "good-token" {
			t.Fatalf("logins %+v, want one with the fresh token", l)
		}
		reqs := creds.requests()
		if len(reqs) != 2 || reqs[0].Fresh || !reqs[1].Fresh {
			t.Fatalf("credential requests %+v, want one plain then one fresh", reqs)
		}
	})
}
