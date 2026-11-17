package auth_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // NTLMv2 is defined over HMAC-MD5.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/crypto/md4" //nolint:gosec,staticcheck // the NT hash is MD4 by definition.

	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The NTLM account the in-process servers verify: DOMAIN\user + the
// account password.
const (
	ntlmDomain = "EXAMPLE"
	ntlmUser   = "alice"
)

// recordedChallenge is the CHALLENGE_MESSAGE an Exchange server sent in the
// [MS-OXIMAP4] section 4.1 example ("Client Successfully Authenticating to a
// Server"), as base64 on the wire. Server challenge 9f388aa866237651, target
// TESTSERVER, NTLMv2 target info, no LM_KEY or KEY_EXCH.
const recordedChallenge = "TlRMTVNTUAACAAAAFAAUADgAAAAFgoqinziKqGYjdlEAAAAAAAAAAGQAZABMAAAABQ" +
	"LODgAAAA9UAEUAUwBUAFMARQBSAFYARQBSAAIAFABUAEUAUwBUAFMARQBSAFYARQBSAA" +
	"EAFABUAEUAUwBUAFMARQBSAFYARQBSAAQAFABUAGUAcwB0AFMAZQByAHYAZQByAAMAFA" +
	"BUAGUAcwB0AFMAZQByAHYAZQByAAAAAAA="

// recordedNegotiate is the NEGOTIATE_MESSAGE from the same example.
const recordedNegotiate = "TlRMTVNTUAABAAAAB4IIogAAAAAAAAAAAAAAAAAAAAAFASgKAAAADw=="

func challengeFixture(t testing.TB) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(recordedChallenge)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ntlmPW(pass string) ports.Password {
	return ports.Password{User: ntlmDomain + `\` + ntlmUser, Secret: []byte(pass)}
}

// ntlmField reads a security buffer (len, maxlen, offset) at off.
func ntlmField(msg []byte, off int) ([]byte, error) {
	if len(msg) < off+8 {
		return nil, errors.New("short message")
	}
	l := int(binary.LittleEndian.Uint16(msg[off:]))
	o := int(binary.LittleEndian.Uint32(msg[off+4:]))
	if o+l > len(msg) {
		return nil, errors.New("field out of range")
	}
	return msg[o : o+l], nil
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[2*i:], r)
	}
	return b
}

func fromUTF16le(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// ntlmNames returns the user and domain of an AUTHENTICATE_MESSAGE.
func ntlmNames(am []byte) (user, domain string, err error) {
	if len(am) < 64 || !bytes.HasPrefix(am, []byte("NTLMSSP\x00\x03\x00\x00\x00")) {
		return "", "", errors.New("not an AUTHENTICATE_MESSAGE")
	}
	d, err := ntlmField(am, 28)
	if err != nil {
		return "", "", err
	}
	u, err := ntlmField(am, 36)
	if err != nil {
		return "", "", err
	}
	return fromUTF16le(u), fromUTF16le(d), nil
}

// verifyNTLMv2 is the server side of [MS-NLMP] section 3.3.2 for the
// recorded challenge: NTProofStr = HMAC-MD5(NTOWFv2, ServerChallenge ||
// temp), NTOWFv2 = HMAC-MD5(MD4(UTF-16LE(password)), UTF-16LE(UPPER(user)
// || domain)); temp must carry the challenge's target info.
func verifyNTLMv2(am []byte, password string) error {
	user, domain, err := ntlmNames(am)
	if err != nil {
		return err
	}
	nt, err := ntlmField(am, 20)
	if err != nil {
		return err
	}
	if len(nt) <= 24 {
		return errors.New("not an NTLMv2 response")
	}
	cm, _ := base64.StdEncoding.DecodeString(recordedChallenge)
	serverChallenge := cm[24:32]
	targetInfo, err := ntlmField(cm, 40)
	if err != nil {
		return err
	}
	proof, temp := nt[:16], nt[16:]
	if temp[0] != 1 || temp[1] != 1 || !bytes.Contains(temp, targetInfo) {
		return errors.New("NTLMv2 blob malformed")
	}
	h := md4.New() //nolint:gosec // the NT hash is MD4 by definition.
	h.Write(utf16le(password))
	m := hmac.New(md5.New, h.Sum(nil))
	m.Write(utf16le(strings.ToUpper(user) + domain))
	ntowf := m.Sum(nil)
	m = hmac.New(md5.New, ntowf)
	m.Write(serverChallenge)
	m.Write(temp)
	if !hmac.Equal(m.Sum(nil), proof) {
		return errReject
	}
	return nil
}

// ntlmServer is SASL NTLM as [MS-OXIMAP4] describes it: an empty
// continuation when the client sent no initial response, the recorded
// CHALLENGE_MESSAGE for its NEGOTIATE_MESSAGE, then verification.
type ntlmServer struct {
	verify func(user, domain string, am []byte) bool
	step   int
}

func (s *ntlmServer) Next(resp []byte) ([]byte, bool, error) {
	switch {
	case s.step == 0 && resp == nil:
		return []byte{}, false, nil
	case s.step == 0:
		s.step++
		if !bytes.HasPrefix(resp, []byte("NTLMSSP\x00\x01\x00\x00\x00")) {
			return nil, true, errReject
		}
		cm, _ := base64.StdEncoding.DecodeString(recordedChallenge)
		return cm, false, nil
	case s.step == 1:
		s.step++
		user, domain, err := ntlmNames(resp)
		if err != nil || !s.verify(user, domain, resp) {
			return nil, true, errReject
		}
		return nil, true, nil
	}
	return nil, true, errReject
}

// TestNTLMRecordedExchange drives the client against the recorded Exchange
// challenge and checks the wire messages.
func TestNTLMRecordedExchange(t *testing.T) {
	c, err := auth.NewClient(auth.NTLM, ntlmPW(password), auth.Offer{}, auth.Target{Host: "mail.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	mech, ir, err := c.Start()
	if err != nil || mech != auth.NTLM {
		t.Fatalf("Start = %q, %v", mech, err)
	}
	// NEGOTIATE as the initial response: same header and message type as
	// the recorded one; no domain or workstation supplied.
	rec, _ := base64.StdEncoding.DecodeString(recordedNegotiate)
	if !bytes.Equal(ir[:12], rec[:12]) || len(ir) < 32 {
		t.Fatalf("negotiate = %x, recorded %x", ir, rec)
	}
	if d, _ := ntlmField(ir, 16); len(d) != 0 {
		t.Errorf("negotiate supplies a domain: %q", d)
	}
	am, err := c.Next(challengeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	user, domain, err := ntlmNames(am)
	if err != nil || user != ntlmUser || domain != ntlmDomain {
		t.Fatalf("authenticate names = %q %q, %v", user, domain, err)
	}
	if err := verifyNTLMv2(am, password); err != nil {
		t.Fatalf("server-side NTLMv2 verification: %v", err)
	}
	if err := verifyNTLMv2(am, "other"); err == nil {
		t.Fatal("verification accepts any password")
	}
	if _, err := c.Next([]byte("again")); err == nil {
		t.Fatal("third step accepted")
	}
}

func TestNTLMRejectsLegacyChallenges(t *testing.T) {
	for name, flag := range map[string]uint32{
		"lm key (NTLMv1)": 0x00000080,
		"key exchange":    0x40000000,
	} {
		t.Run(name, func(t *testing.T) {
			cm := challengeFixture(t)
			binary.LittleEndian.PutUint32(cm[20:], binary.LittleEndian.Uint32(cm[20:])|flag)
			c, err := auth.NewClient(auth.NTLM, ntlmPW(password), auth.Offer{}, auth.Target{})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.Start(); err != nil {
				t.Fatal(err)
			}
			_, err = c.Next(cm)
			wantCode(t, err, model.Unauthorized)
			assertSafe(t, err)
		})
	}
	c, _ := auth.NewClient(auth.NTLM, ntlmPW(password), auth.Offer{}, auth.Target{})
	_, _, _ = c.Start()
	if _, err := c.Next([]byte("not ntlm")); err == nil {
		t.Error("garbage challenge accepted")
	}
}

func TestNTLMNeedsDomainUser(t *testing.T) {
	for _, u := range []string{"alice", "alice@example.com", `\alice`, `EXAMPLE\`, `A\B\c`} {
		if _, err := auth.NewClient(auth.NTLM, ports.Password{User: u, Secret: []byte("p")}, auth.Offer{}, auth.Target{}); err == nil {
			t.Errorf("NewClient(NTLM, %q) accepted", u)
		}
	}
}

// TestConnectNTLM runs the negotiation over real IMAP and SMTP sessions with
// the in-process NTLM server.
func TestConnectNTLM(t *testing.T) {
	exchange := mechSpec{mechs: []string{"GSSAPI", "NTLM", "PLAIN", "LOGIN"}}
	servers := map[string]func(*testing.T, mechSpec) (*testServer, auth.Dialer){
		"imap": func(t *testing.T, s mechSpec) (*testServer, auth.Dialer) {
			ts := newIMAPServer(t, s, implicitTLS)
			return ts, imapDialer(ts.addr)
		},
		"smtp": func(t *testing.T, s mechSpec) (*testServer, auth.Dialer) {
			ts := newSMTPServer(t, s, implicitTLS)
			return ts, smtpDialer(ts.addr)
		},
	}
	for proto, start := range servers {
		t.Run(proto+"/ntlm chosen", func(t *testing.T) {
			ts, d := start(t, exchange)
			sess, err := auth.Connect(t.Context(), tlsConfig(t, proto, d, staticHost(ntlmPW(password))))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sess.Conn.Close() }()
			if sess.Mechanism != auth.NTLM || !equal(ts.attempts.get(), auth.NTLM) {
				t.Fatalf("mechanism %s, attempts %v", sess.Mechanism, ts.attempts.get())
			}
		})
		t.Run(proto+"/wrong password: no downgrade to PLAIN", func(t *testing.T) {
			ts, d := start(t, exchange)
			_, err := auth.Connect(t.Context(), tlsConfig(t, proto, d, staticHost(ntlmPW("wrong-pw"))))
			wantCode(t, err, model.Unauthorized)
			assertSafe(t, err, "wrong-pw")
			if !equal(ts.attempts.get(), auth.NTLM) {
				t.Fatalf("attempts %v", ts.attempts.get())
			}
		})
		t.Run(proto+"/scram beats ntlm", func(t *testing.T) {
			if proto == "imap" {
				t.Skip("the IMAP memory backend knows only the default user; SMTP covers it")
			}
			ts, d := start(t, mechSpec{mechs: []string{"NTLM", "PLAIN", "SCRAM-SHA-256"}})
			p := ntlmPW(password)
			ts.acct.user = p.User // SCRAM verifies the same name
			sess, err := auth.Connect(t.Context(), tlsConfig(t, proto, d, staticHost(p)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sess.Conn.Close() }()
			if sess.Mechanism != auth.SCRAMSHA256 || !equal(ts.attempts.get(), auth.SCRAMSHA256) {
				t.Fatalf("mechanism %s, attempts %v", sess.Mechanism, ts.attempts.get())
			}
		})
		t.Run(proto+"/plain user skips ntlm", func(t *testing.T) {
			ts, d := start(t, exchange)
			sess, err := auth.Connect(t.Context(), tlsConfig(t, proto, d, staticHost(pw())))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sess.Conn.Close() }()
			if sess.Mechanism != auth.Plain || !equal(ts.attempts.get(), auth.Plain) {
				t.Fatalf("mechanism %s, attempts %v", sess.Mechanism, ts.attempts.get())
			}
		})
	}
}

func tlsConfig(t *testing.T, purpose string, d auth.Dialer, h ports.Credentials) auth.Config {
	cfg := config(purpose, d, h)
	cfg.RootCAs = testCerts(t).roots
	return cfg
}

func equal(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !strings.EqualFold(got[i], want[i]) {
			return false
		}
	}
	return true
}
