package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The worked example of "Verifying requests from Slack"
// (https://docs.slack.dev/authentication/verifying-requests-from-slack/):
// signing secret, timestamp, body and the v0 signature it yields.
const (
	docsSecret    = "8f742231b10e8888abcd99yyyzzz85a5" // #nosec G101 -- the secret Slack's documentation prints
	docsTimestamp = "1531420618"
	docsBody      = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V" +
		"&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=" +
		"&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN" +
		"&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
	docsSignature = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
)

func sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + timestamp + ":"))
	m.Write(body)
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

func headers(sig, ts string) http.Header {
	h := http.Header{}
	if sig != "" {
		h.Set(HeaderSignature, sig)
	}
	if ts != "" {
		h.Set(HeaderTimestamp, ts)
	}
	return h
}

func TestSignatureMatchesDocumentedExample(t *testing.T) {
	at := time.Unix(1531420618, 0)
	if got := sign(docsSecret, docsTimestamp, []byte(docsBody)); got != docsSignature {
		t.Fatalf("test helper signs %s, Slack documents %s", got, docsSignature)
	}
	err := verifySignature(headers(docsSignature, docsTimestamp), []byte(docsBody), []byte(docsSecret), at, ReplayWindow)
	if err != nil {
		t.Fatalf("documented request rejected: %v", err)
	}
}

func TestSignatureRejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":"event_callback"}`)
	good := sign("s3cret", ts, body)
	at := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	for name, tc := range map[string]struct {
		h      http.Header
		body   []byte
		secret string
		ok     bool
	}{
		"valid":                  {headers(good, ts), body, "s3cret", true},
		"wrong secret":           {headers(good, ts), body, "other", false},
		"empty secret":           {headers(sign("", ts, body), ts), body, "", false},
		"tampered body":          {headers(good, ts), append([]byte(nil), body[:len(body)-1]...), "s3cret", false},
		"missing signature":      {headers("", ts), body, "s3cret", false},
		"missing timestamp":      {headers(good, ""), body, "s3cret", false},
		"no version prefix":      {headers(good[len("v0="):], ts), body, "s3cret", false},
		"other version":          {headers("v1="+good[len("v0="):], ts), body, "s3cret", false},
		"not hex":                {headers("v0=zz", ts), body, "s3cret", false},
		"timestamp not a number": {headers(sign("s3cret", "soon", body), "soon"), body, "s3cret", false},
		// The window is five minutes either way, as the documentation's
		// example checks; a request exactly on the edge still passes.
		"edge of the window, past":   {headers(sign("s3cret", at(-ReplayWindow), body), at(-ReplayWindow)), body, "s3cret", true},
		"edge of the window, future": {headers(sign("s3cret", at(ReplayWindow), body), at(ReplayWindow)), body, "s3cret", true},
		"replayed past the window":   {headers(sign("s3cret", at(-ReplayWindow-time.Second), body), at(-ReplayWindow-time.Second)), body, "s3cret", false},
		"too far in the future":      {headers(sign("s3cret", at(ReplayWindow+time.Second), body), at(ReplayWindow+time.Second)), body, "s3cret", false},
		// A valid signature does not make a stale timestamp fresh: the
		// timestamp is part of what is signed, so rewriting it breaks it.
		"timestamp rewritten": {headers(good, at(time.Second)), body, "s3cret", false},
	} {
		t.Run(name, func(t *testing.T) {
			err := verifySignature(tc.h, tc.body, []byte(tc.secret), now, ReplayWindow)
			if (err == nil) != tc.ok {
				t.Fatalf("verifySignature = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
