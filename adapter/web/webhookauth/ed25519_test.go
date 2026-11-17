package webhookauth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8032 7.1 test vectors (TEST 1, 2, 3): public key, message and
// signature, through the whole verifier.
func TestEd25519RFC8032Vectors(t *testing.T) {
	for _, tc := range []struct{ name, pub, msg, sig string }{
		{"test 1: empty message",
			"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "",
			"e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"},
		{"test 2: one byte",
			"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c", "72",
			"92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"},
		{"test 3: two bytes",
			"fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025", "af82",
			"6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := webhookauth.Config{Scheme: webhookauth.SchemeEd25519, KeyEncoding: webhookauth.EncodingHex}
			v := mustNew(t, cfg, secrets(tc.pub), webhookauth.Deps{})
			msg := string(unhex(t, tc.msg))
			if err := verify(t, v, hdr("X-Webhook-Signature", tc.sig), msg); err != nil {
				t.Fatalf("known answer rejected: %v", err)
			}
			wantErr(t, verify(t, v, hdr("X-Webhook-Signature", tc.sig), msg+"\x00"), webhookauth.ErrInvalid)
			// S is a scalar below the group order L (RFC 8032 5.1.7): adding
			// L to it yields the same point and must not verify.
			wantErr(t, verify(t, v, hdr("X-Webhook-Signature", malleate(t, tc.sig)), msg), webhookauth.ErrInvalid)
		})
	}
}

// malleate returns sig with S replaced by S+L, the classic malleated form.
func malleate(t *testing.T, sig string) string {
	t.Helper()
	b := unhex(t, sig)
	l := unhex(t, "edd3f55c1a631258d69cf7a2def9de1400000000000000000000000000000010") // L, little-endian
	carry := 0
	for i := 0; i < 32; i++ {
		s := int(b[32+i]) + int(l[i]) + carry
		b[32+i] = byte(s & 0xff)
		carry = s >> 8
	}
	return hex.EncodeToString(b)
}

func TestEd25519EncodingsAndKeys(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id":"1"}`
	sig := ed25519.Sign(priv, []byte(body))
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
		key  string
		sig  string
		want error
	}{
		{"raw key, hex sig", webhookauth.Config{}, string(pub), hex.EncodeToString(sig), nil},
		{"hex key, hex sig", webhookauth.Config{KeyEncoding: webhookauth.EncodingHex}, hex.EncodeToString(pub), hex.EncodeToString(sig), nil},
		{"base64 key, base64 sig", webhookauth.Config{KeyEncoding: webhookauth.EncodingBase64, Encoding: webhookauth.EncodingBase64}, base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(sig), nil},
		{"hex sig under base64", webhookauth.Config{Encoding: webhookauth.EncodingBase64}, string(pub), hex.EncodeToString(sig), webhookauth.ErrMalformed},
		{"prefixed", webhookauth.Config{Prefix: "ed25519="}, string(pub), "ed25519=" + hex.EncodeToString(sig), nil},
		{"short sig", webhookauth.Config{}, string(pub), hex.EncodeToString(sig[:63]), webhookauth.ErrMalformed},
		{"long sig", webhookauth.Config{}, string(pub), hex.EncodeToString(append(sig[:64:64], 0)), webhookauth.ErrMalformed},
		{"zero sig", webhookauth.Config{}, string(pub), hex.EncodeToString(make([]byte, 64)), webhookauth.ErrInvalid},
		// Handing over the private key (64 bytes) or a truncated one is a
		// deployment mistake the verifier refuses, not a key it truncates.
		{"private key as key", webhookauth.Config{}, string(priv), hex.EncodeToString(sig), webhookauth.ErrSecret},
		{"short key", webhookauth.Config{}, string(pub[:31]), hex.EncodeToString(sig), webhookauth.ErrSecret},
		{"empty key", webhookauth.Config{}, "", hex.EncodeToString(sig), webhookauth.ErrSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Scheme = webhookauth.SchemeEd25519
			v := mustNew(t, cfg, secrets(tc.key), webhookauth.Deps{})
			err := verify(t, v, hdr("X-Webhook-Signature", tc.sig), body)
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

func TestEd25519RotationAndWrongKey(t *testing.T) {
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	_, priv3, _ := ed25519.GenerateKey(rand.Reader)
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeEd25519}, secrets(string(pub1), string(pub2)), webhookauth.Deps{})
	for _, k := range []ed25519.PrivateKey{priv1, priv2} {
		if err := verify(t, v, hdr("X-Webhook-Signature", hex.EncodeToString(ed25519.Sign(k, []byte("m")))), "m"); err != nil {
			t.Errorf("rotation key rejected: %v", err)
		}
	}
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", hex.EncodeToString(ed25519.Sign(priv3, []byte("m")))), "m"), webhookauth.ErrInvalid)
}

// Algorithm confusion: the scheme is configuration. An HMAC keyed with the
// published public key, the textbook downgrade, has the wrong shape for
// Ed25519 and is refused; an Ed25519 signature has the wrong shape for HMAC.
func TestNoAlgorithmConfusion(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := "m"
	forged := macHex(sha256New, pub, []byte(body))
	ed := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeEd25519}, secrets(string(pub)), webhookauth.Deps{})
	wantErr(t, verify(t, ed, hdr("X-Webhook-Signature", forged), body), webhookauth.ErrMalformed)
	// Even a forged value of the right length cannot be made by an HMAC.
	wantErr(t, verify(t, ed, hdr("X-Webhook-Signature", forged+forged), body), webhookauth.ErrInvalid)

	hm := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	wantErr(t, verify(t, hm, hdr("X-Webhook-Signature", hex.EncodeToString(ed25519.Sign(priv, []byte(body)))), body), webhookauth.ErrMalformed)
	// The same header cannot carry a bearer credential into a signature scheme.
	wantErr(t, verify(t, ed, http.Header{"Authorization": {"Bearer " + longSecret}}, body), webhookauth.ErrMissing)
}

func TestEd25519Layout(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := "m"
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeEd25519, Payload: "v1\n{body}"}, secrets(string(pub)), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Signature", hex.EncodeToString(ed25519.Sign(priv, []byte("v1\nm")))), body); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", hex.EncodeToString(ed25519.Sign(priv, []byte(body)))), body), webhookauth.ErrInvalid)
}
