package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana/asnAppTag"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/iana/msgtype"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// These tests drive the GSSAPI client past context establishment with a
// known session key, i.e. what the KDC and the server would have agreed on,
// and play the server's half of RFC 4752 and RFC 4121 with gokrb5.

func aesKey(t *testing.T) types.EncryptionKey {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return types.EncryptionKey{KeyType: etypeID.AES256_CTS_HMAC_SHA1_96, KeyValue: k}
}

func established(t *testing.T, authzID string, subkey bool) *gssapiClient {
	t.Helper()
	c := &gssapiClient{
		cred:       ports.Kerberos{Principal: "alice", Realm: "EXAMPLE.TEST", AuthzID: authzID},
		state:      gssapiSecurityLayer,
		sessionKey: aesKey(t),
		auth:       types.Authenticator{SeqNumber: 0x1234567},
		seq:        0x1234567,
	}
	if subkey {
		k := aesKey(t)
		c.subkey = &k
	}
	return c
}

// acceptorWrap is the server's GSS_Wrap(conf_flag=FALSE) of payload.
func acceptorWrap(t *testing.T, key types.EncryptionKey, fl byte, payload []byte) []byte {
	t.Helper()
	wt := gssapi.WrapToken{Flags: fl, EC: 12, SndSeqNum: 77, Payload: payload}
	if err := wt.SetCheckSum(key, keyusage.GSSAPI_ACCEPTOR_SEAL); err != nil {
		t.Fatal(err)
	}
	b, err := wt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// offer is the 4-octet security-layer message: layer bitmask + max size.
func offer(layers byte, maxSize uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, maxSize)
	b[0] = layers
	return b
}

// readChoice verifies the client's wrapped answer as the server would.
func readChoice(t *testing.T, c *gssapiClient, resp []byte) []byte {
	t.Helper()
	var wt gssapi.WrapToken
	if err := wt.Unmarshal(resp, false); err != nil {
		t.Fatalf("answer is not an initiator wrap token: %v", err)
	}
	if ok, err := wt.Verify(c.protectionKey(), keyusage.GSSAPI_INITIATOR_SEAL); !ok {
		t.Fatalf("answer checksum: %v", err)
	}
	if wt.Flags&wrapSealed != 0 {
		t.Error("answer is sealed; RFC 4752 wraps it with conf_flag FALSE")
	}
	if got, want := wt.Flags&wrapAcceptorSubkey != 0, c.subkey != nil; got != want {
		t.Errorf("AcceptorSubkey flag = %v, want %v", got, want)
	}
	if want := uint64(0x1234567); wt.SndSeqNum != want {
		t.Errorf("sequence number = %d, want the authenticator's %d", wt.SndSeqNum, want)
	}
	return wt.Payload
}

func TestGSSAPISecurityLayerChoice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		layers  byte
		authzID string
		subkey  bool
	}{
		{"all layers offered", securityLayerNone | securityLayerIntegrity | securityLayerConf, "", false},
		{"only none offered", securityLayerNone, "", true},
		{"authzid appended", securityLayerNone | securityLayerConf, "shared@example.test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := established(t, tc.authzID, tc.subkey)
			fl := wrapSentByAcceptor
			if tc.subkey {
				fl |= wrapAcceptorSubkey
			}
			resp, err := c.Next(acceptorWrap(t, c.protectionKey(), fl, offer(tc.layers, 0xffffff)))
			if err != nil {
				t.Fatal(err)
			}
			// Octet 1: no security layer; octets 2-4: max buffer 0 (no
			// layer); then the authorization identity, unterminated.
			want := append([]byte{0x01, 0x00, 0x00, 0x00}, tc.authzID...)
			if got := readChoice(t, c, resp); !bytes.Equal(got, want) {
				t.Fatalf("choice = %x, want %x", got, want)
			}
			if _, err := c.Next([]byte{}); err == nil {
				t.Fatal("challenge after the negotiation accepted")
			}
		})
	}
}

func TestGSSAPISecurityLayerRotatedToken(t *testing.T) {
	c := established(t, "", true)
	b := acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor|wrapAcceptorSubkey, offer(securityLayerNone, 0))
	// Rotate payload+checksum right by RRC = 5, as RFC 4121 section 4.2.5
	// allows; the client must undo it.
	data := b[gssapi.HdrLen:]
	r := 5
	rot := append(append([]byte{}, data[len(data)-r:]...), data[:len(data)-r]...)
	binary.BigEndian.PutUint16(b[6:8], uint16(r))
	copy(b[gssapi.HdrLen:], rot)
	if _, err := c.Next(b); err != nil {
		t.Fatal(err)
	}
}

func TestGSSAPISecurityLayerRejects(t *testing.T) {
	cases := map[string]struct {
		subkey bool
		token  func(t *testing.T, c *gssapiClient) []byte
		code   model.ErrorCode
	}{
		"integrity required": {false, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor, offer(securityLayerIntegrity|securityLayerConf, 65536))
		}, model.Unsupported},
		"bad checksum": {false, func(t *testing.T, c *gssapiClient) []byte {
			b := acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor, offer(securityLayerNone, 0))
			b[len(b)-1] ^= 0xff
			return b
		}, model.Unauthorized},
		"tampered offer": {false, func(t *testing.T, c *gssapiClient) []byte {
			b := acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor, offer(securityLayerIntegrity, 0))
			b[gssapi.HdrLen] = securityLayerNone
			return b
		}, model.Unauthorized},
		"wrong key": {false, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, aesKey(t), wrapSentByAcceptor, offer(securityLayerNone, 0))
		}, model.Unauthorized},
		"not from acceptor": {false, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, c.protectionKey(), 0, offer(securityLayerNone, 0))
		}, model.Unauthorized},
		"sealed": {false, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor|wrapSealed, offer(securityLayerNone, 0))
		}, model.Unauthorized},
		"subkey flag without subkey": {false, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor|wrapAcceptorSubkey, offer(securityLayerNone, 0))
		}, model.Unauthorized},
		"session key despite subkey": {true, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, c.sessionKey, wrapSentByAcceptor, offer(securityLayerNone, 0))
		}, model.Unauthorized},
		"offer too long": {false, func(t *testing.T, c *gssapiClient) []byte {
			return acceptorWrap(t, c.protectionKey(), wrapSentByAcceptor, append(offer(securityLayerNone, 0), 'x'))
		}, model.Unauthorized},
		"legacy RFC 1964 token": {false, func(*testing.T, *gssapiClient) []byte {
			return append([]byte{0x60, 0x30, 0x06, 0x09}, make([]byte, 40)...)
		}, model.Unauthorized},
		"short": {false, func(*testing.T, *gssapiClient) []byte { return []byte{0x05, 0x04} }, model.Unauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := established(t, "", tc.subkey)
			_, err := c.Next(tc.token(t, c))
			var me *model.Error
			if !errors.As(err, &me) || me.Code != tc.code {
				t.Fatalf("Next = %v, want %s", err, tc.code)
			}
			if !errors.Is(c.err, err) && c.err != err {
				t.Errorf("client error not recorded: %v", c.err)
			}
		})
	}
}

// apRep builds the server's AP-REP context token for an authenticator.
func apRep(t *testing.T, key types.EncryptionKey, ctime time.Time, cusec int, subkey *types.EncryptionKey) []byte {
	t.Helper()
	part := messages.EncAPRepPart{CTime: ctime, Cusec: cusec}
	if subkey != nil {
		part.Subkey = *subkey
	}
	pb, err := asn1.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	ed, err := crypto.GetEncryptedData(asn1tools.AddASNAppTag(pb, asnAppTag.EncAPRepPart), key, keyusage.AP_REP_ENCPART, 0)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := asn1.Marshal(messages.APRep{PVNO: 5, MsgType: msgtype.KRB_AP_REP, EncPart: ed})
	if err != nil {
		t.Fatal(err)
	}
	oid, _ := asn1.Marshal(gssapi.OIDKRB5.OID())
	return asn1tools.AddASNAppTag(append(append(oid, 0x02, 0x00), asn1tools.AddASNAppTag(rb, asnAppTag.APREP)...), 0)
}

func TestGSSAPIMutualAuthentication(t *testing.T) {
	now := time.Now().UTC()
	pending := func(t *testing.T) *gssapiClient {
		return &gssapiClient{
			state:      gssapiAPRep,
			sessionKey: aesKey(t),
			auth:       types.Authenticator{CTime: now, Cusec: 4242, SeqNumber: 9},
		}
	}
	t.Run("valid AP-REP with subkey", func(t *testing.T) {
		c := pending(t)
		sub := aesKey(t)
		resp, err := c.Next(apRep(t, c.sessionKey, now.Truncate(time.Second), 4242, &sub))
		if err != nil {
			t.Fatal(err)
		}
		// Context complete without an output token: empty, non-nil.
		if resp == nil || len(resp) != 0 {
			t.Fatalf("response = %#v, want empty", resp)
		}
		if c.subkey == nil || !bytes.Equal(c.subkey.KeyValue, sub.KeyValue) || c.state != gssapiSecurityLayer {
			t.Fatal("acceptor subkey not adopted")
		}
	})
	t.Run("valid AP-REP without subkey", func(t *testing.T) {
		c := pending(t)
		if _, err := c.Next(apRep(t, c.sessionKey, now.Truncate(time.Second), 4242, nil)); err != nil {
			t.Fatal(err)
		}
		if c.subkey != nil {
			t.Fatal("subkey invented")
		}
	})
	for name, tok := range map[string]func(c *gssapiClient) []byte{
		"wrong key":   func(*gssapiClient) []byte { return apRep(t, aesKey(t), now.Truncate(time.Second), 4242, nil) },
		"wrong cusec": func(c *gssapiClient) []byte { return apRep(t, c.sessionKey, now.Truncate(time.Second), 1, nil) },
		"wrong ctime": func(c *gssapiClient) []byte {
			return apRep(t, c.sessionKey, now.Add(-time.Minute).Truncate(time.Second), 4242, nil)
		},
		"not a context token": func(*gssapiClient) []byte { return []byte("garbage") },
	} {
		t.Run(name, func(t *testing.T) {
			c := pending(t)
			_, err := c.Next(tok(c))
			var me *model.Error
			if !errors.As(err, &me) || me.Code != model.Unauthorized || c.state != gssapiAPRep {
				t.Fatalf("Next = %v (state %d), want unauthorized", err, c.state)
			}
		})
	}
}

func TestGSSAPIChecksumFlags(t *testing.T) {
	b := gssChecksum(gssapi.ContextFlagMutual | gssapi.ContextFlagInteg)
	if len(b) != 24 || binary.LittleEndian.Uint32(b[0:4]) != 16 {
		t.Fatalf("checksum = %x", b)
	}
	if !bytes.Equal(b[4:20], make([]byte, 16)) {
		t.Error("channel bindings not zero")
	}
	if f := binary.LittleEndian.Uint32(b[20:24]); f != 0x22 {
		t.Errorf("flags = %#x, want mutual|integ", f)
	}
}

func TestKerberosErrorClasses(t *testing.T) {
	for msg, code := range map[string]model.ErrorCode{
		"[Root cause: KDC_Error] KDC_Error: TGS Exchange Error: KRB Error: (7) KDC_ERR_S_PRINCIPAL_UNKNOWN Server not found": model.Permanent,
		"[Root cause: KDC_Error] KRB Error: (24) KDC_ERR_PREAUTH_FAILED":                                                     model.Unauthorized,
		"[Root cause: KDC_Error] KRB Error: (6) KDC_ERR_C_PRINCIPAL_UNKNOWN":                                                 model.Unauthorized,
		"[Root cause: KDC_Error] KRB Error: (37) KRB_AP_ERR_SKEW":                                                            model.Transient,
		"[Root cause: Networking_Error] AS Exchange Error: failed sending AS_REQ to KDC":                                     model.Unavailable,
		"something else": model.Unauthorized,
	} {
		if got := kerberosError(errors.New(msg)); got.Code != code {
			t.Errorf("%q -> %s, want %s", msg, got.Code, code)
		}
	}
	if got := kerberosError(messages.KRBError{ErrorCode: 7}); got.Code != model.Permanent {
		t.Errorf("typed KRBError -> %s", got.Code)
	}
}
