package webhookauth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"net/http"
	"strings"
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
)

func macHex(h func() hash.Hash, key, msg []byte) string {
	m := hmac.New(h, key)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

func sha256New() hash.Hash { return sha256.New() }

// RFC 4231 test vectors, the published known answers for HMAC-SHA-256 and
// HMAC-SHA-512 (RFC 2104 construction), through the whole verifier.
func TestHMACRFC4231Vectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		hash webhookauth.Hash
		key  []byte
		data string
		mac  string
	}{
		{"case 1 sha256", webhookauth.HashSHA256, []byte(strings.Repeat("\x0b", 20)), "Hi There",
			"b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"},
		{"case 1 sha512", webhookauth.HashSHA512, []byte(strings.Repeat("\x0b", 20)), "Hi There",
			"87aa7cdea5ef619d4ff0b4241a1d6cb02379f4e2ce4ec2787ad0b30545e17cdedaa833b7d6b8a702038b274eaea3f4e4be9d914eeb61f1702e696c203a126854"},
		{"case 2 sha256", webhookauth.HashSHA256, []byte("Jefe"), "what do ya want for nothing?",
			"5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"},
		{"case 2 sha512", webhookauth.HashSHA512, []byte("Jefe"), "what do ya want for nothing?",
			"164b7a7bfcf819e2e395fbe73b56e0a387bd64222e831fd610270cd7ea2505549758bf75c05a994a6d034f65f8f0e6fdcaeab1a34d4a6b4b636e070a38bce737"},
		{"case 3 sha256", webhookauth.HashSHA256, []byte(strings.Repeat("\xaa", 20)), strings.Repeat("\xdd", 50),
			"773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe"},
		{"case 6 sha256: key longer than the block", webhookauth.HashSHA256, []byte(strings.Repeat("\xaa", 131)),
			"Test Using Larger Than Block-Size Key - Hash Key First",
			"60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54"},
		{"case 6 sha512: key longer than the block", webhookauth.HashSHA512, []byte(strings.Repeat("\xaa", 131)),
			"Test Using Larger Than Block-Size Key - Hash Key First",
			"80b24263c7c1a3ebb71493c1dd7be8b49b46d1f41b4aeec1121b013783f8f3526b56d037e05f2598bd0fd2215d6a1e5295e64f73f63f0aec8b915a985d786598"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Hash: tc.hash, MinSecretBytes: 1}
			v := mustNew(t, cfg, secrets(string(tc.key)), webhookauth.Deps{})
			if err := verify(t, v, hdr("X-Webhook-Signature", tc.mac), tc.data); err != nil {
				t.Fatalf("known answer rejected: %v", err)
			}
			// One flipped bit in the data or in the MAC never verifies.
			wantErr(t, verify(t, v, hdr("X-Webhook-Signature", tc.mac), tc.data+"."), webhookauth.ErrInvalid)
			bad := []byte(tc.mac)
			bad[0] ^= 1
			wantErr(t, verify(t, v, hdr("X-Webhook-Signature", string(bad)), tc.data), webhookauth.ErrInvalid)
		})
	}
}

func TestHMACEncodingsAndPrefix(t *testing.T) {
	body := `{"id":"m1","text":"hi"}`
	m := hmac.New(sha256.New, []byte(longSecret))
	m.Write([]byte(body))
	mac := m.Sum(nil)
	h := hex.EncodeToString(mac)
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
		hdr  string
		want error
	}{
		{"hex default", webhookauth.Config{}, h, nil},
		{"hex uppercase", webhookauth.Config{}, strings.ToUpper(h), nil},
		{"hex with required prefix", webhookauth.Config{Prefix: "sha256="}, "sha256=" + h, nil},
		{"required prefix absent", webhookauth.Config{Prefix: "sha256="}, h, webhookauth.ErrMalformed},
		{"required prefix of another hash", webhookauth.Config{Prefix: "sha256="}, "sha512=" + h, webhookauth.ErrMalformed},
		{"optional prefix present", webhookauth.Config{Prefix: "sha256=", PrefixOptional: true}, "sha256=" + h, nil},
		{"optional prefix absent", webhookauth.Config{Prefix: "sha256=", PrefixOptional: true}, h, nil},
		{"prefix is not stripped when unconfigured", webhookauth.Config{}, "sha256=" + h, webhookauth.ErrMalformed},
		{"prefix twice", webhookauth.Config{Prefix: "sha256=", PrefixOptional: true}, "sha256=sha256=" + h, webhookauth.ErrMalformed},
		{"base64", webhookauth.Config{Encoding: webhookauth.EncodingBase64}, base64.StdEncoding.EncodeToString(mac), nil},
		{"base64url", webhookauth.Config{Encoding: webhookauth.EncodingBase64URL}, base64.URLEncoding.EncodeToString(mac), nil},
		{"base64 unpadded", webhookauth.Config{Encoding: webhookauth.EncodingBase64Raw}, base64.RawStdEncoding.EncodeToString(mac), nil},
		{"base64url unpadded", webhookauth.Config{Encoding: webhookauth.EncodingBase64URLRaw}, base64.RawURLEncoding.EncodeToString(mac), nil},
		// Encodings never fall back to each other: a hex MAC under a base64
		// configuration decodes to the wrong length and is refused, not
		// guessed at.
		{"hex under base64", webhookauth.Config{Encoding: webhookauth.EncodingBase64}, h, webhookauth.ErrMalformed},
		{"base64 under hex", webhookauth.Config{}, base64.StdEncoding.EncodeToString(mac), webhookauth.ErrMalformed},
		{"padded under unpadded", webhookauth.Config{Encoding: webhookauth.EncodingBase64Raw}, base64.StdEncoding.EncodeToString(mac), webhookauth.ErrMalformed},
		{"url alphabet under std", webhookauth.Config{Encoding: webhookauth.EncodingBase64}, strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(append(mac[:31:31], 0xfb))), webhookauth.ErrMalformed},
		{"odd hex", webhookauth.Config{}, h[:63], webhookauth.ErrMalformed},
		{"truncated mac", webhookauth.Config{}, h[:32], webhookauth.ErrMalformed},
		{"extended mac", webhookauth.Config{}, h + "00", webhookauth.ErrMalformed},
		{"all zero mac", webhookauth.Config{}, strings.Repeat("00", 32), webhookauth.ErrInvalid},
		{"not hex", webhookauth.Config{}, strings.Repeat("zz", 32), webhookauth.ErrMalformed},
		{"whitespace inside", webhookauth.Config{}, h[:10] + " " + h[11:], webhookauth.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Scheme = webhookauth.SchemeHMAC
			cfg.Header = "X-Relay-Signature"
			v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
			err := verify(t, v, hdr("X-Relay-Signature", tc.hdr), body)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			wantErr(t, err, tc.want)
		})
	}
}

func TestHMACBindsExactBody(t *testing.T) {
	body := `{"id":"m1","text":"hi"}`
	sig := macHex(sha256New, []byte(longSecret), []byte(body))
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Signature", sig), body); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// A body re-encoded by a parser (spacing, key order) is another body.
	for _, other := range []string{`{"id": "m1","text":"hi"}`, `{"text":"hi","id":"m1"}`, body + "\n", ""} {
		wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig), other), webhookauth.ErrInvalid)
	}
}

func TestHMACKeyEncodings(t *testing.T) {
	key := []byte(longSecret)
	body := "payload"
	sig := macHex(sha256New, key, []byte(body))
	for _, tc := range []struct {
		name string
		enc  webhookauth.Encoding
		key  string
	}{
		{"raw", webhookauth.EncodingRaw, string(key)},
		{"hex", webhookauth.EncodingHex, hex.EncodeToString(key)},
		{"hex with trailing newline", webhookauth.EncodingHex, hex.EncodeToString(key) + "\n"},
		{"base64", webhookauth.EncodingBase64, base64.StdEncoding.EncodeToString(key)},
		{"base64url unpadded", webhookauth.EncodingBase64URLRaw, base64.RawURLEncoding.EncodeToString(key)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, KeyEncoding: tc.enc}, secrets(tc.key), webhookauth.Deps{})
			if err := verify(t, v, hdr("X-Webhook-Signature", sig), body); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
	// A secret that does not decode, or decodes short, is unusable.
	for _, bad := range []string{"zz", hex.EncodeToString(key[:8]), ""} {
		v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, KeyEncoding: webhookauth.EncodingHex}, secrets(bad), webhookauth.Deps{})
		wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig), body), webhookauth.ErrSecret)
	}
}

func TestHMACFailsClosed(t *testing.T) {
	// The MAC an attacker computes when the key is empty must not verify:
	// an empty or short secret is refused before any comparison.
	body := "x"
	emptyKeyMAC := macHex(sha256New, nil, []byte(body))
	for _, key := range []string{"", "k", strings.Repeat("a", 15)} {
		v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(key), webhookauth.Deps{})
		wantErr(t, verify(t, v, hdr("X-Webhook-Signature", emptyKeyMAC), body), webhookauth.ErrSecret)
		wantErr(t, verify(t, v, hdr("X-Webhook-Signature", macHex(sha256New, []byte(key), []byte(body))), body), webhookauth.ErrSecret)
	}
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, failing(errStoreDown), webhookauth.Deps{})
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", emptyKeyMAC), body), webhookauth.ErrUnavailable)
	wantErr(t, verify(t, mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{}), hdr(), body), webhookauth.ErrMissing)
	wantErr(t, verify(t, mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{}), hdr("X-Webhook-Signature", ""), body), webhookauth.ErrMissing)
}

func TestHMACDuplicateSignatureHeader(t *testing.T) {
	body := "x"
	good := macHex(sha256New, []byte(longSecret), []byte(body))
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	h := http.Header{"X-Webhook-Signature": {good, good}}
	wantErr(t, verify(t, v, h, body), webhookauth.ErrMalformed)
}

func TestHMACRotation(t *testing.T) {
	prev := strings.Repeat("r", 32)
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret, prev), webhookauth.Deps{})
	for _, k := range []string{longSecret, prev} {
		if err := verify(t, v, hdr("X-Webhook-Signature", macHex(sha256New, []byte(k), []byte("b"))), "b"); err != nil {
			t.Errorf("key %q.. rejected: %v", k[:3], err)
		}
	}
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", macHex(sha256New, []byte(strings.Repeat("z", 32)), []byte("b"))), "b"), webhookauth.ErrInvalid)
}

func TestHMACComparesFixedSizeMACsForEveryKey(t *testing.T) {
	var sizes []int
	restore := webhookauth.SpyEqual(func(a, b []byte) { sizes = append(sizes, len(a), len(b)) })
	defer restore()
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret, strings.Repeat("r", 32)), webhookauth.Deps{})
	_ = verify(t, v, hdr("X-Webhook-Signature", macHex(sha256New, []byte(longSecret), []byte("b"))), "b")
	if len(sizes) != 4 {
		t.Fatalf("%d comparisons, want one per key even after a match", len(sizes)/2)
	}
	for _, n := range sizes {
		if n != sha256.Size {
			t.Fatalf("compared %d bytes, want %d", n, sha256.Size)
		}
	}
}

func TestPayloadLayouts(t *testing.T) {
	body := `{"a":1}`
	for _, tc := range []struct {
		name, payload, signed string
	}{
		{"body only", "", body},
		{"explicit body", "{body}", body},
		{"prefix and body", "v0:{body}", "v0:" + body},
		{"escaped braces", "{{{body}}}", "{" + body + "}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Payload: tc.payload}, secrets(longSecret), webhookauth.Deps{})
			sig := macHex(sha256New, []byte(longSecret), []byte(tc.signed))
			if err := verify(t, v, hdr("X-Webhook-Signature", sig), body); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			wantErr(t, verify(t, v, hdr("X-Webhook-Signature", macHex(sha256New, []byte(longSecret), []byte(body+"x"))), body), webhookauth.ErrInvalid)
		})
	}
}

func TestSignatureConfigErrors(t *testing.T) {
	src := secrets(longSecret)
	hm := func(c webhookauth.Config) webhookauth.Config { c.Scheme = webhookauth.SchemeHMAC; return c }
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
	}{
		{"unknown hash", hm(webhookauth.Config{Hash: "md5"})},
		{"sha1 is not offered", hm(webhookauth.Config{Hash: "sha1"})},
		{"unknown encoding", hm(webhookauth.Config{Encoding: "base32"})},
		{"raw signature encoding", hm(webhookauth.Config{Encoding: webhookauth.EncodingRaw})},
		{"unknown key encoding", hm(webhookauth.Config{KeyEncoding: "rot13"})},
		{"optional prefix without prefix", hm(webhookauth.Config{PrefixOptional: true})},
		{"prefix with a control character", hm(webhookauth.Config{Prefix: "sha256=\n"})},
		{"payload without body", hm(webhookauth.Config{Payload: "v0:"})},
		{"payload with body twice", hm(webhookauth.Config{Payload: "{body}{body}"})},
		{"payload with unknown placeholder", hm(webhookauth.Config{Payload: "{body}{nonce}"})},
		{"payload with unbalanced brace", hm(webhookauth.Config{Payload: "{body"})},
		{"payload with stray close", hm(webhookauth.Config{Payload: "{body}}"})},
		{"payload on bearer", webhookauth.Config{Scheme: webhookauth.SchemeBearer, Payload: "{body}"}},
		{"hash on token", webhookauth.Config{Scheme: webhookauth.SchemeToken, Hash: webhookauth.HashSHA256}},
		{"encoding on token", webhookauth.Config{Scheme: webhookauth.SchemeToken, Encoding: webhookauth.EncodingHex}},
		{"prefix on bearer", webhookauth.Config{Scheme: webhookauth.SchemeBearer, Prefix: "x"}},
		{"hash on ed25519", webhookauth.Config{Scheme: webhookauth.SchemeEd25519, Hash: webhookauth.HashSHA256}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhookauth.New(tc.cfg, src, webhookauth.Deps{})
			wantErr(t, err, webhookauth.ErrConfig)
		})
	}
}

// Decoding is strict to the byte: an encoded form the sender did not write
// canonically is refused, never normalised, so one MAC has one spelling.
func TestSignatureEncodingIsCanonicalOnly(t *testing.T) {
	body := "x"
	mac := hmac.New(sha256.New, []byte(longSecret))
	mac.Write([]byte(body))
	raw := mac.Sum(nil)
	canonical := base64.StdEncoding.EncodeToString(raw)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	i := strings.IndexByte(alphabet, canonical[len(canonical)-2])
	// Same bytes, non-zero trailing bits (RFC 4648 3.5): a lenient decoder
	// reads it as the genuine MAC.
	loose := canonical[:len(canonical)-2] + string(alphabet[i|1]) + "="
	if loose == canonical {
		t.Fatal("test needs a different spelling")
	}
	if dec, err := base64.StdEncoding.DecodeString(loose); err != nil || !hmac.Equal(dec, raw) {
		t.Fatalf("test premise: %v", err)
	}
	cfg := webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Encoding: webhookauth.EncodingBase64}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Signature", canonical), body); err != nil {
		t.Fatalf("canonical: %v", err)
	}
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", loose), body), webhookauth.ErrMalformed)
	// The std decoder skips line breaks; a hand-built header with one is
	// still not the canonical text.
	withBreak := canonical[:10] + "\n" + canonical[10:]
	wantErr(t, verify(t, v, http.Header{"X-Webhook-Signature": {withBreak}}, body), webhookauth.ErrMalformed)
	withCR := canonical[:10] + "\r\n" + canonical[10:]
	wantErr(t, verify(t, v, http.Header{"X-Webhook-Signature": {withCR}}, body), webhookauth.ErrMalformed)
}

func TestKeyWithInnerLineBreakIsUnusable(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte(longSecret))
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, KeyEncoding: webhookauth.EncodingBase64}, secrets(enc[:20]+"\n"+enc[20:]), webhookauth.Deps{})
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sigOf("x")), "x"), webhookauth.ErrSecret)
}
