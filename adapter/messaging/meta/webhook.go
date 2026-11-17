package meta

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"strings"
)

// SignatureHeader carries the payload signature of every webhook event
// notification: "sha256=" followed by the hex HMAC-SHA256 of the raw
// request body under the app secret.
const SignatureHeader = "X-Hub-Signature-256"

const signaturePrefix = "sha256="

// Sign returns the SignatureHeader value for body under appSecret, as Meta
// computes it.
func Sign(appSecret, body []byte) string {
	m := hmac.New(sha256.New, appSecret)
	m.Write(body)
	return signaturePrefix + hex.EncodeToString(m.Sum(nil))
}

// ValidSignature reports whether header is the signature of the raw body
// under appSecret. The comparison is constant time over the decoded MAC.
// An empty secret, a missing prefix or a malformed hex value is invalid.
// body must be the bytes as received: Meta signs the escaped payload, so a
// re-encoded body never verifies.
func ValidSignature(header string, body, appSecret []byte) bool {
	if len(appSecret) == 0 {
		return false
	}
	hexMAC, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok || len(hexMAC) != 2*sha256.Size {
		return false
	}
	got, err := hex.DecodeString(hexMAC)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, appSecret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// Subscription verification query parameters.
const (
	ParamMode        = "hub.mode"
	ParamChallenge   = "hub.challenge"
	ParamVerifyToken = "hub.verify_token"
	modeSubscribe    = "subscribe"
)

// VerifyChallenge checks a subscription verification request: hub.mode is
// "subscribe" and hub.verify_token equals verifyToken, compared in constant
// time. It returns the hub.challenge to echo back as the response body, and
// false when the request must be refused. An empty verifyToken never
// verifies, and the challenge must be the integer Meta sends (ASCII
// digits), so nothing else is ever echoed.
func VerifyChallenge(q url.Values, verifyToken []byte) (string, bool) {
	challenge := q.Get(ParamChallenge)
	if len(verifyToken) == 0 || !isDigits(challenge) || q.Get(ParamMode) != modeSubscribe {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(q.Get(ParamVerifyToken)), verifyToken) != 1 {
		return "", false
	}
	return challenge, true
}

func isDigits(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
