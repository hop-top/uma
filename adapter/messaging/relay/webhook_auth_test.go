package relay_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

func sign(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// A gateway already in the field keeps its header names, signature prefix
// (sometimes absent) and unsigned RFC 3339 timestamp: configuration alone.
func TestWebhookLegacyHMACConvention(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = relay.Auth{
		Scheme: "hmac", Header: "X-Relay-Signature", Prefix: "sha256=", PrefixOptional: true,
		Payload:   "{body}",
		Timestamp: relay.AuthTimestamp{Header: "X-Relay-Timestamp", Format: "unix_or_rfc3339", AllowUnsigned: true},
	}
	stamp := testNow.Format(time.RFC3339)
	signed := func(body, prefix string, ts string) call {
		return call{body: body, header: http.Header{
			"X-Relay-Signature": {prefix + sign(webhookSecret, body)}, "X-Relay-Timestamp": {ts},
		}}
	}
	tests := []struct {
		name string
		c    call
		want int
	}{
		{"prefixed", signed(hello, "sha256=", stamp), http.StatusOK},
		{"unprefixed", signed(hello, "", stamp), http.StatusOK},
		{"unix seconds", signed(hello, "sha256=", strconv.FormatInt(testNow.Unix(), 10)), http.StatusOK},
		{"wrong key", call{body: hello, header: http.Header{"X-Relay-Signature": {"sha256=" + sign("other-secret-0123456789abcdef", hello)}, "X-Relay-Timestamp": {stamp}}}, http.StatusUnauthorized},
		{"body altered", call{body: hello + " ", header: signed(hello, "sha256=", stamp).header}, http.StatusUnauthorized},
		{"stale", signed(hello, "sha256=", testNow.Add(-time.Hour).Format(time.RFC3339)), http.StatusUnauthorized},
		{"no timestamp", call{body: hello, header: http.Header{"X-Relay-Signature": {"sha256=" + sign(webhookSecret, hello)}}}, http.StatusUnauthorized},
		{"malformed signature", call{body: hello, header: http.Header{"X-Relay-Signature": {"sha256=zz"}, "X-Relay-Timestamp": {stamp}}}, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &sink{}
			rec := serve(t, newInstance(t, cfg, nil), s, tt.c)
			if rec.Code != tt.want {
				t.Fatalf("answer %d, want %d", rec.Code, tt.want)
			}
			if (tt.want == http.StatusOK) != (len(s.ingresses()) == 1) {
				t.Fatalf("%d ingresses", len(s.ingresses()))
			}
		})
	}
}

func replayConfig() relay.Config {
	cfg := baseConfig()
	cfg.Auth = relay.Auth{
		Scheme: "hmac", Header: "X-Relay-Signature",
		Timestamp: relay.AuthTimestamp{Header: "X-Relay-Timestamp"},
		Replay:    relay.AuthReplay{Source: "signature"},
	}
	return cfg
}

func replayCall(body string) call {
	ts := strconv.FormatInt(testNow.Unix(), 10)
	return call{body: body, header: http.Header{
		"X-Relay-Signature": {sign(webhookSecret, ts+"."+body)}, "X-Relay-Timestamp": {ts},
	}}
}

func TestWebhookReplayIsRefused(t *testing.T) {
	inst := newInstance(t, replayConfig(), nil)
	s := &sink{}
	if rec := serve(t, inst, s, replayCall(hello)); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := serve(t, inst, s, replayCall(hello)); rec.Code != http.StatusConflict {
		t.Fatalf("replay: %d", rec.Code)
	}
	if len(s.ingresses()) != 1 {
		t.Fatalf("%d ingresses", len(s.ingresses()))
	}
}

// A delivery that was not accepted, however it failed, is released: replay
// rejection is for deliveries the engine took, so the relay's retry (or the
// same bytes after a configuration fix) is a new attempt, not a replay.
func TestWebhookFailedDeliveryReleasesTheReplayReservation(t *testing.T) {
	tests := []struct {
		name string
		s    *sink
		body string
		want int
	}{
		{"retryable sink failure", &sink{err: model.NewError(model.Unavailable, "down")}, hello, http.StatusServiceUnavailable},
		{"permanent sink failure", &sink{err: errors.New("no")}, hello, http.StatusUnprocessableEntity},
		{"unusable message", &sink{}, `{"from":"+1"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := newInstance(t, replayConfig(), nil)
			if rec := serve(t, inst, tt.s, replayCall(tt.body)); rec.Code != tt.want {
				t.Fatalf("first: %d, want %d", rec.Code, tt.want)
			}
			// The same bytes again reach the sink, not the replay check.
			if rec := serve(t, inst, tt.s, replayCall(tt.body)); rec.Code != tt.want {
				t.Fatalf("again: %d, want %d (a replay is 409)", rec.Code, tt.want)
			}
		})
	}
}

// Channels do not share replay keys: the same delivery to a second channel
// of one factory is its own.
func TestWebhookReplayScopedPerChannel(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	other := channelOf(replayConfig())
	other.Ref.ID = "00000000-0000-7000-8000-00000000c0a2"
	a := open(t, f, replayConfig(), depsWith(newCreds()))
	b, err := f.Open(t.Context(), other, depsWith(newCreds()))
	if err != nil {
		t.Fatal(err)
	}
	if rec := serve(t, a, &sink{}, replayCall(hello)); rec.Code != http.StatusOK {
		t.Fatalf("a: %d", rec.Code)
	}
	if rec := serve(t, b.(*relay.Instance), &sink{}, replayCall(hello)); rec.Code != http.StatusOK {
		t.Fatalf("b: %d", rec.Code)
	}
}
