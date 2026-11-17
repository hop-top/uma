package zendesk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"testing"
	"time"
)

// testSigningSecret is the static secret Zendesk documents for signing
// test webhooks before a webhook exists.
const testSigningSecret = "dGhpc19zZWNyZXRfaXNfZm9yX3Rlc3Rpbmdfb25seQ==" // #nosec G101 -- documented public test secret

func signFor(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	now := time.Date(2025, 1, 15, 6, 13, 30, 0, time.UTC)
	body := []byte(`{"type":"zen:event-type:ticket.comment_added"}`)
	ts := "2025-01-15T06:13:27Z"
	good := signFor(testSigningSecret, ts, body)
	header := func(sig, stamp string) http.Header {
		h := http.Header{}
		if sig != "" {
			h.Set(SignatureHeader, sig)
		}
		if stamp != "" {
			h.Set(SignatureTimestampHeader, stamp)
		}
		return h
	}
	tolerance := 5 * time.Minute
	for _, c := range []struct {
		name   string
		h      http.Header
		body   []byte
		secret string
		now    time.Time
		ok     bool
	}{
		{"valid", header(good, ts), body, testSigningSecret, now, true},
		{"valid at the tolerance edge", header(good, ts), body, testSigningSecret, now.Add(tolerance - 3*time.Second), true},
		{"missing signature", header("", ts), body, testSigningSecret, now, false},
		{"missing timestamp", header(good, ""), body, testSigningSecret, now, false},
		{"other secret", header(good, ts), body, "another-secret", now, false},
		{"body changed", header(good, ts), []byte(`{"type":"zen:event-type:ticket.merged"}`), testSigningSecret, now, false},
		{"timestamp changed", header(good, "2025-01-15T06:13:28Z"), body, testSigningSecret, now, false},
		{"signature not base64", header("%%%", ts), body, testSigningSecret, now, false},
		{"signature truncated", header(good[:20], ts), body, testSigningSecret, now, false},
		{"hex instead of base64", header("6162", ts), body, testSigningSecret, now, false},
		{"stale beyond the window", header(good, ts), body, testSigningSecret, now.Add(tolerance), false},
		{"from the future beyond the window", header(good, ts), body, testSigningSecret, now.Add(-tolerance - 10*time.Second), false},
		{"timestamp not RFC 3339", header(signFor(testSigningSecret, "1736921607", body), "1736921607"), body, testSigningSecret, now, false},
		{"empty secret", header(good, ts), body, "", now, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := verifySignature(c.h, c.body, []byte(c.secret), c.now, tolerance)
			if (err == nil) != c.ok {
				t.Fatalf("verifySignature = %v, want ok=%v", err, c.ok)
			}
		})
	}
}
