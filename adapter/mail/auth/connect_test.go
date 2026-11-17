package auth_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-sasl"

	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Mechanism sets from live provider probes.
var profiles = map[string]struct{ imap, smtp mechSpec }{
	"gmail": {
		imap: mechSpec{mechs: []string{"XOAUTH2", "OAUTHBEARER", "PLAIN"}},
		smtp: mechSpec{mechs: []string{"PLAIN", "LOGIN", "XOAUTH2", "OAUTHBEARER"}},
	},
	"m365": {
		imap: mechSpec{mechs: []string{"XOAUTH2"}, loginDisabled: true},
		smtp: mechSpec{mechs: []string{"LOGIN", "XOAUTH2"}},
	},
	"dovecot": {imap: mechSpec{mechs: dovecotMechs}, smtp: mechSpec{mechs: dovecotMechs}},
	"scram-no-plus": {
		imap: mechSpec{mechs: []string{"CRAM-MD5", "SCRAM-SHA-1", "SCRAM-SHA-256", "PLAIN"}},
		smtp: mechSpec{mechs: []string{"CRAM-MD5", "SCRAM-SHA-1", "SCRAM-SHA-256", "PLAIN"}},
	},
	"scram-sha1": {
		imap: mechSpec{mechs: []string{"PLAIN", "SCRAM-SHA-1"}},
		smtp: mechSpec{mechs: []string{"PLAIN", "SCRAM-SHA-1"}},
	},
	"cram-only":               {imap: mechSpec{mechs: []string{"CRAM-MD5"}}, smtp: mechSpec{mechs: []string{"CRAM-MD5"}}},
	"cram-only-logindisabled": {imap: mechSpec{mechs: []string{"CRAM-MD5"}, loginDisabled: true}},
	"login-only":              {imap: mechSpec{mechs: []string{"LOGIN"}}, smtp: mechSpec{mechs: []string{"LOGIN"}}},
	// No SASL at all: only the IMAP LOGIN command.
	"imap-login-command": {imap: mechSpec{mechs: []string{}}, smtp: mechSpec{mechs: []string{}}},
}

func newServer(t *testing.T, proto string, spec mechSpec, sec security) (*testServer, auth.Dialer) {
	t.Helper()
	if proto == "imap" {
		ts := newIMAPServer(t, spec, sec)
		return ts, imapDialer(ts.addr)
	}
	ts := newSMTPServer(t, spec, sec)
	return ts, smtpDialer(ts.addr)
}

// TestNegotiationMatrix authenticates against in-process IMAP and SMTP
// servers advertising each provider's mechanism set, per credential kind.
func TestNegotiationMatrix(t *testing.T) {
	cert := ports.ClientCert{Certificate: testCerts(t).client}
	sharedOAuth := oauth("good-token")
	sharedOAuth.AuthzID = "shared@example.com"
	pwShared := pw()
	pwShared.AuthzID = "shared@example.com"
	creds := map[string]ports.Credential{
		"oauth": oauth("good-token"), "oauth-shared": sharedOAuth,
		"password": pw(), "password-authzid": pwShared, "cert": cert,
	}
	cases := []struct {
		profile, proto, cred, want string // want "" = model.Unsupported
	}{
		{"gmail", "imap", "oauth", auth.OAuthBearer},
		{"gmail", "imap", "oauth-shared", auth.OAuthBearer},
		{"gmail", "imap", "password", auth.Plain},
		{"gmail", "imap", "cert", ""},
		{"gmail", "smtp", "oauth", auth.OAuthBearer},
		{"gmail", "smtp", "password", auth.Plain},
		{"m365", "imap", "oauth", auth.XOAuth2},
		{"m365", "imap", "oauth-shared", auth.XOAuth2},
		{"m365", "imap", "password", ""},
		{"m365", "smtp", "oauth", auth.XOAuth2},
		{"m365", "smtp", "password", auth.Login},
		{"m365", "smtp", "password-authzid", ""},
		{"m365", "smtp", "cert", ""},
		{"dovecot", "imap", "oauth", auth.OAuthBearer},
		{"dovecot", "imap", "password", auth.SCRAMSHA256Plus},
		{"dovecot", "imap", "password-authzid", auth.SCRAMSHA256Plus},
		{"dovecot", "imap", "cert", auth.External},
		{"dovecot", "smtp", "oauth", auth.OAuthBearer},
		{"dovecot", "smtp", "password", auth.SCRAMSHA256Plus},
		{"dovecot", "smtp", "cert", auth.External},
		{"scram-no-plus", "imap", "password", auth.SCRAMSHA256},
		{"scram-no-plus", "smtp", "password", auth.SCRAMSHA256},
		{"scram-sha1", "imap", "password", auth.SCRAMSHA1},
		{"scram-sha1", "smtp", "password", auth.SCRAMSHA1},
		{"cram-only", "imap", "password", auth.IMAPLogin},
		{"cram-only-logindisabled", "imap", "password", ""},
		{"cram-only", "smtp", "password", ""},
		{"login-only", "imap", "password", auth.Login},
		{"login-only", "smtp", "password", auth.Login},
		{"login-only", "imap", "oauth", ""},
		{"imap-login-command", "imap", "password", auth.IMAPLogin},
		{"imap-login-command", "imap", "password-authzid", ""},
		{"imap-login-command", "smtp", "password", ""},
	}
	for _, tc := range cases {
		t.Run(tc.profile+"/"+tc.proto+"/"+tc.cred, func(t *testing.T) {
			spec := profiles[tc.profile].imap
			if tc.proto == "smtp" {
				spec = profiles[tc.profile].smtp
			}
			ts, d := newServer(t, tc.proto, spec, implicitTLS)
			cfg := config(tc.proto, d, staticHost(creds[tc.cred]))
			cfg.RootCAs = testCerts(t).roots
			s, err := auth.Connect(context.Background(), cfg)
			if tc.want == "" {
				wantCode(t, err, model.Unsupported)
				if got := ts.attempts.get(); len(got) != 0 {
					t.Fatalf("server saw AUTH %v, want none", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Connect = %v", err)
			}
			defer func() { _ = s.Conn.Close() }()
			if s.Mechanism != tc.want || s.Kind != creds[tc.cred].Kind() {
				t.Fatalf("session = %s/%s, want %s", s.Mechanism, s.Kind, tc.want)
			}
			if got := ts.attempts.get(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("server saw AUTH %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestPlaintextRefused: no AUTH on a plaintext connection unless the peer is
// loopback and the channel opted in.
func TestPlaintextRefused(t *testing.T) {
	spec := mechSpec{mechs: []string{"PLAIN", "LOGIN"}}
	for _, proto := range []string{"imap", "smtp"} {
		t.Run(proto+"/no opt-in", func(t *testing.T) {
			ts, d := newServer(t, proto, spec, plaintext)
			d.TLS, d.StartTLS = nil, nil
			_, err := auth.Connect(context.Background(), config(proto, d, staticHost(pw())))
			wantCode(t, err, model.Unsupported)
			if got := ts.attempts.get(); len(got) != 0 {
				t.Fatalf("server saw AUTH %v", got)
			}
		})
		t.Run(proto+"/tls dialer returning plaintext", func(t *testing.T) {
			// An adapter bug (or a stripped STARTTLS) must still not send
			// credentials: the check runs on the connection, not the dialer.
			ts, d := newServer(t, proto, spec, plaintext)
			d.TLS, d.StartTLS = func(ctx context.Context, _ *tls.Config) (auth.Conn, error) { return d.Plain(ctx) }, nil
			_, err := auth.Connect(context.Background(), config(proto, d, staticHost(pw())))
			wantCode(t, err, model.Unsupported)
			if got := ts.attempts.get(); len(got) != 0 {
				t.Fatalf("server saw AUTH %v", got)
			}
		})
		t.Run(proto+"/loopback opt-in", func(t *testing.T) {
			ts, d := newServer(t, proto, spec, plaintext)
			d.TLS, d.StartTLS = nil, nil
			cfg := config(proto, d, staticHost(pw()))
			cfg.Policy.AllowPlaintextLoopback = true
			s, err := auth.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Conn.Close()
			if s.Mechanism != auth.Plain || !slices.Equal(ts.attempts.get(), []string{"PLAIN"}) {
				t.Fatalf("mechanism %s, server saw %v", s.Mechanism, ts.attempts.get())
			}
		})
	}
	t.Run("opt-in, non-loopback peer", func(t *testing.T) {
		fc := &fakeConn{mechs: []string{"PLAIN"}, addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 143}}
		d := auth.Dialer{Plain: func(context.Context) (auth.Conn, error) { return fc, nil }}
		cfg := config("imap", d, staticHost(pw()))
		cfg.Policy.AllowPlaintextLoopback = true
		_, err := auth.Connect(context.Background(), cfg)
		wantCode(t, err, model.Unsupported)
		if !fc.closed {
			t.Error("refused connection not closed")
		}
	})
}

func TestCheckTransport(t *testing.T) {
	tls13 := tlsState(t)
	cases := []struct {
		name  string
		state *tls.ConnectionState
		addr  net.Addr
		optIn bool
		ok    bool
	}{
		{"tls", tls13, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}, false, true},
		{"tls handshake incomplete", &tls.ConnectionState{}, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}, false, false},
		{"plain remote", nil, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}, true, false},
		{"plain loopback, no opt-in", nil, loopback, false, false},
		{"plain 127.0.0.1 opt-in", nil, loopback, true, true},
		{"plain 127.8.9.10 opt-in", nil, &net.TCPAddr{IP: net.IPv4(127, 8, 9, 10)}, true, true},
		{"plain ::1 opt-in", nil, &net.TCPAddr{IP: net.IPv6loopback}, true, true},
		{"plain v4-mapped loopback opt-in", nil, &net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1")}, true, true},
		{"plain string addr opt-in", nil, stringAddr("127.0.0.1:1025"), true, true},
		{"plain hostname opt-in", nil, stringAddr("localhost:1025"), true, false},
		{"plain nil addr opt-in", nil, nil, true, false},
	}
	for _, tc := range cases {
		err := auth.CheckTransport(&fakeConn{state: tc.state, addr: tc.addr}, auth.Policy{AllowPlaintextLoopback: tc.optIn})
		if (err == nil) != tc.ok {
			t.Errorf("%s: CheckTransport = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

// TestNoDowngradeAfterRejection: a refused mechanism ends the attempt; no
// weaker mechanism follows and a password is never re-resolved.
func TestNoDowngradeAfterRejection(t *testing.T) {
	wrong := ports.Password{User: user, Secret: []byte("wrong-password")}
	cases := []struct{ profile, proto, mech string }{
		{"dovecot", "imap", auth.SCRAMSHA256Plus},
		{"dovecot", "smtp", auth.SCRAMSHA256Plus},
		{"scram-no-plus", "smtp", auth.SCRAMSHA256},
		{"gmail", "imap", auth.Plain},
		{"m365", "smtp", auth.Login},
		{"imap-login-command", "imap", auth.IMAPLogin},
	}
	for _, tc := range cases {
		t.Run(tc.profile+"/"+tc.proto, func(t *testing.T) {
			spec := profiles[tc.profile].imap
			if tc.proto == "smtp" {
				spec = profiles[tc.profile].smtp
			}
			ts, d := newServer(t, tc.proto, spec, implicitTLS)
			h := staticHost(wrong)
			cfg := config(tc.proto, d, h)
			cfg.RootCAs = testCerts(t).roots
			_, err := auth.Connect(context.Background(), cfg)
			wantCode(t, err, model.Unauthorized)
			assertSafe(t, err, "wrong-password")
			if got := ts.attempts.get(); !slices.Equal(got, []string{tc.mech}) {
				t.Fatalf("server saw AUTH %v, want only [%s]", got, tc.mech)
			}
			if n := len(h.requests()); n != 1 {
				t.Fatalf("resolves = %d, want 1", n)
			}
		})
	}
}

// TestOAuthFreshRetry: one Fresh resolve, one reconnect, one retry with the
// same mechanism; a second rejection is unauthorized with no retry advice.
func TestOAuthFreshRetry(t *testing.T) {
	for _, tc := range []struct{ profile, proto, mech string }{
		{"m365", "imap", auth.XOAuth2},
		{"m365", "smtp", auth.XOAuth2},
		{"gmail", "imap", auth.OAuthBearer},
		{"gmail", "smtp", auth.OAuthBearer},
	} {
		spec := profiles[tc.profile].imap
		if tc.proto == "smtp" {
			spec = profiles[tc.profile].smtp
		}
		t.Run(tc.profile+"/"+tc.proto+"/recovers", func(t *testing.T) {
			ts, d := newServer(t, tc.proto, spec, implicitTLS)
			h := rotatingHost(oauth("stale-token"), oauth("fresh-token"))
			cfg := config(tc.proto, d, h)
			cfg.RootCAs = testCerts(t).roots
			s, err := auth.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Conn.Close()
			reqs := h.requests()
			if len(reqs) != 2 || reqs[0].Fresh || !reqs[1].Fresh {
				t.Fatalf("resolves = %+v, want [stale, fresh]", reqs)
			}
			if got := ts.attempts.get(); !slices.Equal(got, []string{tc.mech, tc.mech}) {
				t.Fatalf("server saw AUTH %v", got)
			}
		})
		t.Run(tc.profile+"/"+tc.proto+"/gives up", func(t *testing.T) {
			ts, d := newServer(t, tc.proto, spec, implicitTLS)
			h := staticHost(oauth("stale-token"))
			cfg := config(tc.proto, d, h)
			cfg.RootCAs = testCerts(t).roots
			_, err := auth.Connect(context.Background(), cfg)
			wantCode(t, err, model.Unauthorized)
			var me *model.Error
			if errors.As(err, &me); me.Retry != model.NoRetry {
				t.Errorf("retry = %s, want none", me.Retry)
			}
			if n := len(h.requests()); n != 2 {
				t.Fatalf("resolves = %d, want 2", n)
			}
			if got := ts.attempts.get(); !slices.Equal(got, []string{tc.mech, tc.mech}) {
				t.Fatalf("server saw AUTH %v, want two %s", got, tc.mech)
			}
		})
	}
}

// TestRetryKeepsMechanism: the fresh retry must use the rejected mechanism;
// a reconnect offering something else is refused.
func TestRetryKeepsMechanism(t *testing.T) {
	tls13 := tlsState(t)
	reject := func(sasl.Client) error { return auth.ErrRejected }
	conns := []*fakeConn{
		{mechs: []string{"OAUTHBEARER", "XOAUTH2"}, state: tls13, addr: loopback, auth: reject},
		{mechs: []string{"XOAUTH2"}, state: tls13, addr: loopback, auth: func(sasl.Client) error { return nil }},
	}
	n := 0
	d := auth.Dialer{TLS: func(context.Context, *tls.Config) (auth.Conn, error) { c := conns[n]; n++; return c, nil }}
	_, err := auth.Connect(context.Background(), config("imap", d, rotatingHost(oauth("stale-token"), oauth("fresh-token"))))
	wantCode(t, err, model.Unauthorized)
	if n != 2 || !conns[0].closed || !conns[1].closed {
		t.Fatalf("dials %d, closed %v %v", n, conns[0].closed, conns[1].closed)
	}
}

// TestUnsupportedCredential: a kind mail cannot use, or one outside Accept,
// fails before any connection.
func TestUnsupportedCredential(t *testing.T) {
	cases := map[string]struct {
		cred   ports.Credential
		accept []ports.CredentialKind
	}{
		"secret accepted":         {ports.Secret{Bytes: []byte("api-key-123")}, []ports.CredentialKind{ports.KindSecret}},
		"password not in accept":  {pw(), []ports.CredentialKind{ports.KindOAuth2}},
		"nil credential":          {nil, mailKinds},
		"nil pointer credential":  {(*ports.Password)(nil), mailKinds},
		"oauth on password relay": {oauth("good-token"), []ports.CredentialKind{ports.KindPassword}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dials := 0
			d := auth.Dialer{TLS: func(context.Context, *tls.Config) (auth.Conn, error) {
				dials++
				return nil, errors.New("unexpected dial")
			}}
			cfg := config("smtp.submission", d, staticHost(tc.cred))
			cfg.Request.Accept = tc.accept
			_, err := auth.Connect(context.Background(), cfg)
			wantCode(t, err, model.Unsupported)
			assertSafe(t, err, "api-key-123")
			if dials != 0 {
				t.Fatalf("dialed %d times", dials)
			}
		})
	}
	t.Run("secret on a server", func(t *testing.T) {
		ts, d := newServer(t, "smtp", mechSpec{mechs: dovecotMechs}, implicitTLS)
		cfg := config("smtp", d, staticHost(ports.Secret{Bytes: []byte("api-key-123")}))
		cfg.Request.Accept = []ports.CredentialKind{ports.KindSecret}
		cfg.RootCAs = testCerts(t).roots
		_, err := auth.Connect(context.Background(), cfg)
		wantCode(t, err, model.Unsupported)
		if got := ts.attempts.get(); len(got) != 0 {
			t.Fatalf("server saw AUTH %v", got)
		}
	})
}

func TestResolveErrors(t *testing.T) {
	d := auth.Dialer{TLS: func(context.Context, *tls.Config) (auth.Conn, error) { return nil, errors.New("unexpected dial") }}
	cases := map[string]struct {
		err  error
		code model.ErrorCode
	}{
		"host unauthorized": {model.NewError(model.Unauthorized, "consent revoked"), model.Unauthorized},
		"host unsupported":  {model.NewError(model.Unsupported, "kind unavailable"), model.Unsupported},
		"raw store error":   {errors.New("vault: token hunter2 expired"), model.Unavailable},
	}
	for name, tc := range cases {
		h := &host{resolve: func(ports.CredentialRequest) (ports.Credential, error) { return nil, tc.err }}
		_, err := auth.Connect(context.Background(), config("imap", d, h))
		if !model.IsCode(err, tc.code) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
		assertSafe(t, err, "hunter2")
	}
	h := &host{resolve: func(ports.CredentialRequest) (ports.Credential, error) { return nil, context.Canceled }}
	if _, err := auth.Connect(context.Background(), config("imap", d, h)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	d := smtpDialer("127.0.0.1:1")
	cases := map[string]func(*auth.Config){
		"no credentials port": func(c *auth.Config) { c.Credentials = nil },
		"empty accept":        func(c *auth.Config) { c.Request.Accept = nil },
		"negative margin":     func(c *auth.Config) { c.Policy.ReconnectMargin = -time.Second },
		"no dialer":           func(c *auth.Config) { c.Dialer = auth.Dialer{} },
	}
	for name, mutate := range cases {
		cfg := config("imap", d, staticHost(pw()))
		mutate(&cfg)
		_, err := auth.Connect(context.Background(), cfg)
		if !model.IsCode(err, model.Validation) {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
}

// TestTransportFallback: implicit TLS first; STARTTLS only when the implicit
// port refuses the connection, never after a failed TLS handshake.
func TestTransportFallback(t *testing.T) {
	p := testCerts(t)
	spec := mechSpec{mechs: []string{"PLAIN"}}
	closedAddr := func() string {
		ln := listen(t)
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}

	t.Run("implicit preferred", func(t *testing.T) {
		tlsSrv := newSMTPServer(t, spec, implicitTLS)
		stSrv := newSMTPServer(t, spec, startTLS)
		d := auth.Dialer{TLS: smtpDialer(tlsSrv.addr).TLS, StartTLS: smtpDialer(stSrv.addr).StartTLS}
		cfg := config("smtp.submission", d, staticHost(pw()))
		cfg.RootCAs = p.roots
		s, err := auth.Connect(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Conn.Close()
		if len(tlsSrv.attempts.get()) != 1 || len(stSrv.attempts.get()) != 0 {
			t.Fatalf("implicit %v, starttls %v", tlsSrv.attempts.get(), stSrv.attempts.get())
		}
	})
	t.Run("starttls when implicit refused", func(t *testing.T) {
		stSrv := newSMTPServer(t, spec, startTLS)
		d := auth.Dialer{TLS: smtpDialer(closedAddr()).TLS, StartTLS: smtpDialer(stSrv.addr).StartTLS}
		cfg := config("smtp.submission", d, staticHost(pw()))
		cfg.RootCAs = p.roots
		s, err := auth.Connect(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Conn.Close() }()
		if st, ok := s.Conn.TLS(); !ok || !st.HandshakeComplete {
			t.Fatal("session not on TLS")
		}
		if !slices.Equal(stSrv.attempts.get(), []string{"PLAIN"}) {
			t.Fatalf("starttls server saw %v", stSrv.attempts.get())
		}
	})
	t.Run("starttls only", func(t *testing.T) {
		stSrv := newSMTPServer(t, spec, startTLS)
		d := smtpDialer(stSrv.addr)
		d.TLS = nil
		cfg := config("smtp.submission", d, staticHost(pw()))
		cfg.RootCAs = p.roots
		s, err := auth.Connect(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Conn.Close()
	})
	t.Run("no fallback after handshake failure", func(t *testing.T) {
		// The implicit port answers with a certificate from an unknown CA.
		bad := listen(t)
		bad = tls.NewListener(bad, serverTLS(t, p.untrusted))
		go func() {
			for {
				c, err := bad.Accept()
				if err != nil {
					return
				}
				go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
			}
		}()
		t.Cleanup(func() { _ = bad.Close() })
		stSrv := newSMTPServer(t, spec, startTLS)
		d := auth.Dialer{TLS: smtpDialer(bad.Addr().String()).TLS, StartTLS: smtpDialer(stSrv.addr).StartTLS}
		cfg := config("smtp.submission", d, staticHost(pw()))
		cfg.RootCAs = p.roots
		_, err := auth.Connect(context.Background(), cfg)
		wantCode(t, err, model.Unavailable)
		if got := stSrv.attempts.get(); len(got) != 0 {
			t.Fatalf("fell back to STARTTLS: %v", got)
		}
	})
	t.Run("starttls not offered", func(t *testing.T) {
		plainSrv := newSMTPServer(t, spec, plaintext)
		d := smtpDialer(plainSrv.addr)
		d.TLS = nil
		cfg := config("smtp.submission", d, staticHost(pw()))
		_, err := auth.Connect(context.Background(), cfg)
		wantCode(t, err, model.Unavailable)
		if got := plainSrv.attempts.get(); len(got) != 0 {
			t.Fatalf("server saw AUTH %v", got)
		}
	})
}

// TestSCRAMServerProof: a server that accepts without proving knowledge of
// the password (no valid server-final) fails the session.
func TestSCRAMServerProof(t *testing.T) {
	tls13 := tlsState(t)
	cases := map[string]func(sasl.Client) error{
		"success without server-final": func(c sasl.Client) error {
			if _, _, err := c.Start(); err != nil {
				return err
			}
			return nil
		},
		"forged server-final": func(c sasl.Client) error {
			_, ir, err := c.Start()
			if err != nil {
				return err
			}
			nonce := string(ir[len(ir)-24:])
			if _, err := c.Next([]byte("r=" + nonce + "srv,s=c2FsdA==,i=4096")); err != nil {
				return err
			}
			_, err = c.Next([]byte("v=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="))
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			fc := &fakeConn{mechs: []string{"SCRAM-SHA-256"}, state: tls13, addr: loopback, auth: run}
			d := auth.Dialer{TLS: func(context.Context, *tls.Config) (auth.Conn, error) { return fc, nil }}
			_, err := auth.Connect(context.Background(), config("imap", d, staticHost(pw())))
			wantCode(t, err, model.Unauthorized)
			if !fc.closed {
				t.Error("connection not closed")
			}
		})
	}
}

func TestTransportErrorIsNotRejection(t *testing.T) {
	tls13 := tlsState(t)
	fc := &fakeConn{mechs: []string{"XOAUTH2"}, state: tls13, addr: loopback,
		auth: func(sasl.Client) error { return errors.New("read: connection reset; token good-token") }}
	d := auth.Dialer{TLS: func(context.Context, *tls.Config) (auth.Conn, error) { return fc, nil }}
	h := staticHost(oauth("good-token"))
	_, err := auth.Connect(context.Background(), config("imap", d, h))
	wantCode(t, err, model.Transient)
	if n := len(h.requests()); n != 1 {
		t.Fatalf("resolves = %d; a transport error must not trigger a fresh retry", n)
	}
}

func TestReconnectAt(t *testing.T) {
	ts, d := newServer(t, "imap", profiles["m365"].imap, implicitTLS)
	_ = ts
	for _, tc := range []struct {
		margin time.Duration
		want   time.Time
	}{
		{0, expiry.Add(-5 * time.Minute)},
		{2 * time.Minute, expiry.Add(-2 * time.Minute)},
	} {
		cfg := config("imap", d, staticHost(oauth("good-token")))
		cfg.RootCAs = testCerts(t).roots
		cfg.Policy.ReconnectMargin = tc.margin
		s, err := auth.Connect(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Conn.Close()
		if !s.ReconnectAt.Equal(tc.want) {
			t.Errorf("margin %v: ReconnectAt = %v, want %v", tc.margin, s.ReconnectAt, tc.want)
		}
	}
	if got := (auth.Policy{}).ReconnectAt(pw()); !got.IsZero() {
		t.Errorf("password ReconnectAt = %v, want zero", got)
	}
	if auth.DefaultReconnectMargin != 5*time.Minute {
		t.Errorf("default margin = %v", auth.DefaultReconnectMargin)
	}
}

func TestPurposeInMessages(t *testing.T) {
	_, err := auth.Choose(pw(), auth.Offer{Mechanisms: []string{"XOAUTH2"}})
	if err == nil {
		t.Fatal("want error")
	}
	d := auth.Dialer{Plain: func(context.Context) (auth.Conn, error) { return nil, errors.New("x") }}
	_, err = auth.Connect(context.Background(), config("smtp.submission", d, staticHost(pw())))
	var me *model.Error
	if !errors.As(err, &me) || me.SafeMessage[:len("smtp.submission: ")] != "smtp.submission: " {
		t.Fatalf("err = %v, want purpose prefix", err)
	}
}
