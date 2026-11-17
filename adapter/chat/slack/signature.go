package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Request signing, as "Verifying requests from Slack" documents it
// (https://docs.slack.dev/authentication/verifying-requests-from-slack/):
// X-Slack-Signature is "v0=" and the hex HMAC-SHA256, keyed with the
// app's signing secret, of "v0:" + X-Slack-Request-Timestamp + ":" + the
// raw request body. Every request Slack sends to the app is signed so:
// Events API deliveries, interactions and slash commands alike.
const (
	HeaderSignature = "X-Slack-Signature"
	HeaderTimestamp = "X-Slack-Request-Timestamp"
)

// ReplayWindow is how far the request timestamp may differ from now, in
// either direction. It is the five minutes the documentation's own check
// uses ("abs(time.time() - timestamp) > 60 * 5" rejects).
const ReplayWindow = 5 * time.Minute

const signatureVersion = "v0"

var errBadSignature = errors.New("slack: request signature rejected")

// verifySignature checks a request's signature over its raw body in
// constant time and then its timestamp against the replay window. The
// timestamp is part of what is signed, so a replay cannot refresh it. Any
// missing, malformed or mismatching value, and an empty secret, is
// rejected with the same error.
func verifySignature(h http.Header, body, secret []byte, now time.Time, window time.Duration) error {
	sig, stamp := h.Get(HeaderSignature), h.Get(HeaderTimestamp)
	if len(secret) == 0 || sig == "" || stamp == "" {
		return errBadSignature
	}
	hexSig, ok := strings.CutPrefix(sig, signatureVersion+"=")
	if !ok {
		return errBadSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil || len(got) == 0 {
		return errBadSignature
	}
	secs, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return errBadSignature
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(signatureVersion + ":" + stamp + ":"))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return errBadSignature
	}
	if d := now.Sub(time.Unix(secs, 0)); d > window || d < -window {
		return errBadSignature
	}
	return nil
}
