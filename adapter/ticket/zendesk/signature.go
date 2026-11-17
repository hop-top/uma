package zendesk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"time"
)

// Webhook signing, as Zendesk documents it: the signature header carries
// base64(HMAC-SHA256(secret, timestamp + body)), where timestamp is the
// exact value of the timestamp header and body the raw request body.
const (
	SignatureHeader          = "X-Zendesk-Webhook-Signature"
	SignatureTimestampHeader = "X-Zendesk-Webhook-Signature-Timestamp"
)

var errBadSignature = errors.New("zendesk: webhook signature rejected")

// verifySignature checks a webhook request's signature in constant time and
// then its timestamp against the replay window: the timestamp may differ
// from now by at most tolerance in either direction. Any missing, malformed
// or mismatching value is rejected with the same error.
func verifySignature(h http.Header, body, secret []byte, now time.Time, tolerance time.Duration) error {
	sig, stamp := h.Get(SignatureHeader), h.Get(SignatureTimestampHeader)
	if len(secret) == 0 || sig == "" || stamp == "" {
		return errBadSignature
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return errBadSignature
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(stamp))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return errBadSignature
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return errBadSignature
	}
	if d := now.Sub(at); d > tolerance || d < -tolerance {
		return errBadSignature
	}
	return nil
}
