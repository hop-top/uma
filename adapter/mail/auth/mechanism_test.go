package auth_test

import (
	"crypto/tls"
	"slices"
	"strconv"
	"strings"
	"testing"

	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

var dovecotMechs = []string{"PLAIN", "LOGIN", "CRAM-MD5", "SCRAM-SHA-1", "SCRAM-SHA-256",
	"SCRAM-SHA-256-PLUS", "OAUTHBEARER", "XOAUTH2", "EXTERNAL", "GSSAPI", "NTLM"}

// exchangeMechs is what an on-premises Exchange server advertises.
var exchangeMechs = []string{"GSSAPI", "NTLM", "PLAIN", "LOGIN"}

// testTarget is a target NewClient accepts for every kind.
var testTarget = auth.Target{Host: "localhost", Kerberos: auth.KerberosConfig{Service: "imap"}}

func domainPW() ports.Password {
	return ports.Password{User: `CORP\alice`, Secret: []byte(password)}
}

func krb() ports.Kerberos {
	return ports.Kerberos{Principal: "alice", Realm: "EXAMPLE.TEST", Keytab: []byte("keytab")}
}

// TestChooseMatrix pins the negotiation order per credential kind: kind ∩
// advertised, strongest first, CRAM-MD5 never.
func TestChooseMatrix(t *testing.T) {
	tls13 := tlsState(t)
	cert := ports.ClientCert{Certificate: testCerts(t).client}
	pwAs := ports.Password{User: user, Secret: []byte(password), AuthzID: "shared@example.com"}
	cases := []struct {
		name  string
		cred  ports.Credential
		offer auth.Offer
		want  string // "" = model.Unsupported
	}{
		// OAuth2
		{"oauth/full", oauth("t"), auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, auth.OAuthBearer},
		{"oauth/gmail-imap", oauth("t"), auth.Offer{Mechanisms: []string{"XOAUTH2", "OAUTHBEARER", "PLAIN"}, TLS: tls13}, auth.OAuthBearer},
		{"oauth/m365", oauth("t"), auth.Offer{Mechanisms: []string{"LOGIN", "XOAUTH2"}, TLS: tls13}, auth.XOAuth2},
		{"oauth/lowercase", oauth("t"), auth.Offer{Mechanisms: []string{"xoauth2"}, TLS: tls13}, auth.XOAuth2},
		{"oauth/password-only server", oauth("t"), auth.Offer{Mechanisms: []string{"PLAIN", "LOGIN"}, TLS: tls13, LoginCommand: true}, ""},
		{"oauth/gssapi offered first", oauth("t"), auth.Offer{Mechanisms: []string{"GSSAPI", "NTLM", "XOAUTH2"}, TLS: tls13}, auth.XOAuth2},
		{"oauth/exchange without oauth", oauth("t"), auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, ""},
		// Password
		{"password/full", pw(), auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, auth.SCRAMSHA256Plus},
		{"password/plus without channel binding", pw(), auth.Offer{Mechanisms: dovecotMechs}, auth.SCRAMSHA256},
		{"password/scram-256", pw(), auth.Offer{Mechanisms: []string{"PLAIN", "SCRAM-SHA-1", "SCRAM-SHA-256"}, TLS: tls13}, auth.SCRAMSHA256},
		{"password/scram-1", pw(), auth.Offer{Mechanisms: []string{"PLAIN", "LOGIN", "SCRAM-SHA-1"}, TLS: tls13}, auth.SCRAMSHA1},
		{"password/plain", pw(), auth.Offer{Mechanisms: []string{"LOGIN", "PLAIN"}, TLS: tls13}, auth.Plain},
		{"password/login", pw(), auth.Offer{Mechanisms: []string{"LOGIN", "XOAUTH2"}, TLS: tls13}, auth.Login},
		{"password/imap login command", pw(), auth.Offer{TLS: tls13, LoginCommand: true}, auth.IMAPLogin},
		{"password/imap sasl beats login command", pw(), auth.Offer{Mechanisms: []string{"PLAIN"}, TLS: tls13, LoginCommand: true}, auth.Plain},
		{"password/m365 imap LOGINDISABLED", pw(), auth.Offer{Mechanisms: []string{"XOAUTH2"}, TLS: tls13}, ""},
		{"password/cram-md5 only", pw(), auth.Offer{Mechanisms: []string{"CRAM-MD5"}, TLS: tls13}, ""},
		{"password/cram-md5 and login", pw(), auth.Offer{Mechanisms: []string{"CRAM-MD5", "LOGIN"}, TLS: tls13}, auth.Login},
		{"password/nothing", pw(), auth.Offer{TLS: tls13}, ""},
		{"password-authzid/plain", pwAs, auth.Offer{Mechanisms: []string{"LOGIN", "PLAIN"}, TLS: tls13}, auth.Plain},
		{"password-authzid/login only", pwAs, auth.Offer{Mechanisms: []string{"LOGIN"}, TLS: tls13}, ""},
		{"password-authzid/imap login only", pwAs, auth.Offer{TLS: tls13, LoginCommand: true}, ""},
		{"password-authzid/full", pwAs, auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, auth.SCRAMSHA256Plus},
		// NTLM: after SCRAM, before PLAIN, only for DOMAIN\user.
		{"password-domain/exchange", domainPW(), auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, auth.NTLM},
		{"password-domain/scram beats ntlm", domainPW(), auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, auth.SCRAMSHA256Plus},
		{"password-domain/scram-1 beats ntlm", domainPW(), auth.Offer{Mechanisms: []string{"NTLM", "PLAIN", "SCRAM-SHA-1"}, TLS: tls13}, auth.SCRAMSHA1},
		{"password-domain/no ntlm", domainPW(), auth.Offer{Mechanisms: []string{"GSSAPI", "PLAIN"}, TLS: tls13}, auth.Plain},
		{"password/exchange plain user", pw(), auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, auth.Plain},
		{"password/exchange upn user", ports.Password{User: "alice@corp.example", Secret: []byte(password)}, auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, auth.Plain},
		{"password/ntlm only, plain user", pw(), auth.Offer{Mechanisms: []string{"NTLM"}, TLS: tls13}, ""},
		{"password/ntlm only, empty domain", ports.Password{User: `\alice`, Secret: []byte(password)}, auth.Offer{Mechanisms: []string{"NTLM"}, TLS: tls13}, ""},
		{"password/ntlm only, empty user", ports.Password{User: `CORP\`, Secret: []byte(password)}, auth.Offer{Mechanisms: []string{"NTLM"}, TLS: tls13}, ""},
		{"password-domain-authzid/exchange", ports.Password{User: `CORP\alice`, Secret: []byte(password), AuthzID: "shared"}, auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, auth.Plain},
		// Kerberos
		{"kerberos/exchange", krb(), auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, auth.GSSAPI},
		{"kerberos/full", krb(), auth.Offer{Mechanisms: dovecotMechs, TLS: tls13, LoginCommand: true}, auth.GSSAPI},
		{"kerberos/no gssapi", krb(), auth.Offer{Mechanisms: []string{"NTLM", "PLAIN", "XOAUTH2"}, TLS: tls13, LoginCommand: true}, ""},
		{"pointer kerberos", &ports.Kerberos{Principal: "alice", Realm: "R", CCache: []byte("c")}, auth.Offer{Mechanisms: exchangeMechs, TLS: tls13}, auth.GSSAPI},
		// ClientCert
		{"cert/full", cert, auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, auth.External},
		{"cert/plaintext", cert, auth.Offer{Mechanisms: dovecotMechs}, ""},
		{"cert/no external", cert, auth.Offer{Mechanisms: []string{"PLAIN", "XOAUTH2"}, TLS: tls13}, ""},
		// Kinds mail cannot use
		{"secret/full", ports.Secret{Bytes: []byte("x")}, auth.Offer{Mechanisms: dovecotMechs, TLS: tls13, LoginCommand: true}, ""},
		{"nil", nil, auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, ""},
		// Pointers behave like values.
		{"pointer password", &ports.Password{User: user, Secret: []byte(password)}, auth.Offer{Mechanisms: []string{"PLAIN"}, TLS: tls13}, auth.Plain},
		{"nil pointer", (*ports.OAuth2)(nil), auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.Choose(tc.cred, tc.offer)
			if tc.want == "" {
				wantCode(t, err, model.Unsupported)
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Choose = %q, %v; want %q", got, err, tc.want)
			}
			if got == auth.IMAPLogin {
				return
			}
			if _, err := auth.NewClient(got, tc.cred, tc.offer, testTarget); err != nil {
				t.Fatalf("NewClient(%s) = %v", got, err)
			}
		})
	}
}

// TestChooseEveryMechanism runs each mechanism alone so each preference
// entry is reachable and nothing else is ever chosen.
func TestChooseEveryMechanism(t *testing.T) {
	tls13 := tlsState(t)
	creds := map[ports.CredentialKind]ports.Credential{
		ports.KindOAuth2:     oauth("t"),
		ports.KindPassword:   domainPW(),
		ports.KindClientCert: ports.ClientCert{Certificate: testCerts(t).client},
		ports.KindSecret:     ports.Secret{Bytes: []byte("x")},
		ports.KindKerberos:   krb(),
	}
	all := append([]string{"CRAM-MD5", "DIGEST-MD5", "ANONYMOUS"}, dovecotMechs...)
	for _, mech := range all {
		for kind, cred := range creds {
			offer := auth.Offer{Mechanisms: []string{mech}, TLS: tls13}
			got, err := auth.Choose(cred, offer)
			usable := false
			for _, m := range auth.Preference(kind) {
				usable = usable || m == mech
			}
			if usable {
				if err != nil || got != mech {
					t.Errorf("%s/%s: Choose = %q, %v", kind, mech, got, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s/%s: Choose = %q, want unsupported", kind, mech, got)
			}
		}
	}
	if p := auth.Preference(ports.KindPassword); slices.Contains(p, "CRAM-MD5") {
		t.Errorf("password preference includes CRAM: %v", p)
	}
}

func TestChooseUnsupportedMessage(t *testing.T) {
	_, err := auth.Choose(pw(), auth.Offer{Mechanisms: []string{"XOAUTH2", "evil\r\nX", "CRAM-MD5"}})
	wantCode(t, err, model.Unsupported)
	msg := err.Error()
	if !strings.Contains(msg, "XOAUTH2 CRAM-MD5") || strings.Contains(msg, "evil") {
		t.Errorf("message = %q", msg)
	}
}

func TestNewClientInitialResponses(t *testing.T) {
	tls13 := tlsState(t)
	shared := oauth("tok-1")
	shared.AuthzID = "shared@example.com"
	cases := []struct {
		name   string
		mech   string
		cred   ports.Credential
		offer  auth.Offer
		prefix string
	}{
		{"xoauth2", auth.XOAuth2, oauth("tok-1"), auth.Offer{}, "user=" + user + "\x01auth=Bearer tok-1\x01\x01"},
		{"xoauth2 shared mailbox", auth.XOAuth2, shared, auth.Offer{}, "user=shared@example.com\x01auth=Bearer tok-1\x01\x01"},
		{"oauthbearer", auth.OAuthBearer, oauth("tok-1"), auth.Offer{}, "n,a=" + user + ",\x01host=localhost\x01auth=Bearer tok-1\x01\x01"},
		{"plain", auth.Plain, pw(), auth.Offer{}, "\x00" + user + "\x00" + password},
		{"external", auth.External, ports.ClientCert{AuthzID: "a@b"}, auth.Offer{TLS: tls13}, "a@b"},
		{"scram plus binds", auth.SCRAMSHA256Plus, pw(), auth.Offer{Mechanisms: dovecotMechs, TLS: tls13}, "p=tls-exporter,,n=" + user + ",r="},
		{"scram y: tls, no plus offered", auth.SCRAMSHA256, pw(), auth.Offer{Mechanisms: []string{"SCRAM-SHA-256"}, TLS: tls13}, "y,,n="},
		{"scram n: plaintext", auth.SCRAMSHA256, pw(), auth.Offer{Mechanisms: []string{"SCRAM-SHA-256"}}, "n,,n="},
		{"scram n: tls state without keying material", auth.SCRAMSHA1, pw(), auth.Offer{Mechanisms: []string{"SCRAM-SHA-1"}, TLS: &tls.ConnectionState{}}, "n,,n="},
		{"scram-1 n: plus offered but sha-1-plus unsupported", auth.SCRAMSHA1, pw(), auth.Offer{Mechanisms: []string{"SCRAM-SHA-1", "SCRAM-SHA-1-PLUS"}, TLS: tls13}, "n,,n="},
		{"scram authzid", auth.SCRAMSHA256, ports.Password{User: user, Secret: []byte(password), AuthzID: "shared@example.com"}, auth.Offer{}, "n,a=shared@example.com,n="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := auth.NewClient(tc.mech, tc.cred, tc.offer, auth.Target{Host: "localhost"})
			if err != nil {
				t.Fatal(err)
			}
			mech, ir, err := c.Start()
			if err != nil || mech != tc.mech || !strings.HasPrefix(string(ir), tc.prefix) {
				t.Fatalf("Start = %q, %q, %v; want %q prefix %q", mech, ir, err, tc.mech, tc.prefix)
			}
		})
	}
	if _, err := auth.NewClient(auth.SCRAMSHA256Plus, pw(), auth.Offer{Mechanisms: dovecotMechs}, auth.Target{}); err == nil {
		t.Error("SCRAM PLUS without channel binding: want error")
	}
	if _, err := auth.NewClient("CRAM-MD5", pw(), auth.Offer{}, auth.Target{}); err == nil {
		t.Error("CRAM-MD5: want error")
	}
	if _, err := auth.NewClient(auth.XOAuth2, pw(), auth.Offer{}, auth.Target{}); err == nil {
		t.Error("XOAUTH2 with a password: want error")
	}
}

// TestTokenResolvedPerStart pins that the OAuth clients fetch the token on
// every Start, i.e. every (re)connect.
func TestTokenResolvedPerStart(t *testing.T) {
	for name, mk := range map[string]func(auth.TokenFunc) interface {
		Start() (string, []byte, error)
	}{
		"xoauth2": func(f auth.TokenFunc) interface {
			Start() (string, []byte, error)
		} {
			return auth.NewXOAuth2Client(user, f)
		},
		"oauthbearer": func(f auth.TokenFunc) interface {
			Start() (string, []byte, error)
		} {
			return auth.NewOAuthBearerClient(user, "imap.example.com", f)
		},
	} {
		n := 0
		c := mk(func() (string, error) { n++; return "tok-" + strconv.Itoa(n), nil })
		_, ir1, _ := c.Start()
		_, ir2, _ := c.Start()
		if n != 2 || !strings.Contains(string(ir1), "tok-1") || !strings.Contains(string(ir2), "tok-2") {
			t.Errorf("%s: token calls %d, ir %q / %q", name, n, ir1, ir2)
		}
	}
}
