package webhookauth

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"strings"
)

func (v *Verifier) verifySignature(ctx context.Context, req Request, in inputs) (*Result, error) {
	raw, err := headerValue(req.Header, v.plan.header)
	if err != nil {
		return nil, err
	}
	sigs, err := v.decodeSignatures(raw)
	if err != nil {
		return nil, err
	}
	keys, err := v.keys(ctx)
	if err != nil {
		return nil, err
	}
	msg := v.plan.layout.bytes(fields{body: req.Body, timestamp: in.ts.raw, id: in.id})
	var ok []bool
	if v.plan.Scheme == SchemeHMAC {
		ok = v.checkHMAC(keys, msg, sigs)
	} else {
		ok = checkEd25519(keys, msg, sigs)
	}
	matched := distinctMatches(sigs, ok)
	if len(matched) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, v.plan.header)
	}
	return &Result{Scheme: v.plan.Scheme, sigs: matched}, nil
}

// decodeSignature strips the configured prefix and decodes the value under
// the configured encoding. The decoded length must be the size the scheme
// produces, which also rejects a signature made by another algorithm.
func (v *Verifier) decodeSignature(raw string) ([]byte, error) {
	if p := v.plan.Prefix; p != "" {
		rest, ok := strings.CutPrefix(raw, p)
		if !ok && !v.plan.PrefixOptional {
			return nil, fmt.Errorf("%w: %s lacks prefix %q", ErrMalformed, v.plan.header, p)
		}
		if ok {
			raw = rest
		}
	}
	sig, err := v.plan.Encoding.decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not %s", ErrMalformed, v.plan.header, v.plan.Encoding)
	}
	want := ed25519.SignatureSize
	if v.plan.Scheme == SchemeHMAC {
		want = v.newHash()().Size()
	}
	if len(sig) != want {
		return nil, fmt.Errorf("%w: %s is %d bytes, want %d", ErrMalformed, v.plan.header, len(sig), want)
	}
	return sig, nil
}

func (v *Verifier) newHash() func() hash.Hash {
	if v.plan.Hash == HashSHA512 {
		return sha512.New
	}
	return sha256.New
}

// distinctMatches returns the signatures that verified, each once.
func distinctMatches(sigs [][]byte, ok []bool) [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	for i, sig := range sigs {
		if ok[i] && !seen[string(sig)] {
			seen[string(sig)] = true
			out = append(out, sig)
		}
	}
	return out
}

// checkHMAC recomputes the MAC under every key and compares it with every
// signature, in constant time and without stopping at a match (RFC 2104
// construction). ok[i] says whether sigs[i] matched under some key.
func (v *Verifier) checkHMAC(keys [][]byte, msg []byte, sigs [][]byte) []bool {
	ok := make([]bool, len(sigs))
	for _, k := range keys {
		m := hmac.New(v.newHash(), k)
		m.Write(msg)
		mac := m.Sum(nil)
		for i, sig := range sigs {
			if equalFixed(sig, mac) == 1 {
				ok[i] = true
			}
		}
	}
	return ok
}

// checkEd25519 verifies every signature over msg (RFC 8032 5.1.7: S below
// the group order, cofactorless equation) under every public key.
func checkEd25519(keys [][]byte, msg []byte, sigs [][]byte) []bool {
	ok := make([]bool, len(sigs))
	for _, k := range keys {
		for i, sig := range sigs {
			if ed25519.Verify(ed25519.PublicKey(k), msg, sig) {
				ok[i] = true
			}
		}
	}
	return ok
}
