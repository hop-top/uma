package webhookauth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
)

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func hmacWithTimestamp(cfg webhookauth.Config) webhookauth.Config {
	cfg.Scheme = webhookauth.SchemeHMAC
	cfg.Timestamp.Header = "X-Relay-Timestamp"
	cfg.Timestamp.Tolerance = time.Minute
	return cfg
}

func signedAt(ts, body string) string {
	return macHex(sha256New, []byte(longSecret), []byte(ts+"."+body))
}

// The window is inclusive and symmetric: a clock running behind the sender
// and one running ahead are both tolerated up to the tolerance, no further.
func TestTimestampWindowTable(t *testing.T) {
	v := mustNew(t, hmacWithTimestamp(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{})
	for _, tc := range []struct {
		name string
		ts   time.Time
		want error
	}{
		{"now", now, nil},
		{"past edge", now.Add(-time.Minute), nil},
		{"past edge plus 1s", now.Add(-time.Minute - time.Second), webhookauth.ErrStale},
		{"future edge", now.Add(time.Minute), nil},
		{"future edge plus 1s", now.Add(time.Minute + time.Second), webhookauth.ErrStale},
		{"10 minutes old", now.Add(-10 * time.Minute), webhookauth.ErrStale},
		{"10 minutes ahead", now.Add(10 * time.Minute), webhookauth.ErrStale},
		{"a year old", now.AddDate(-1, 0, 0), webhookauth.ErrStale},
		{"epoch", time.Unix(0, 0), webhookauth.ErrStale},
		{"far future", now.AddDate(100, 0, 0), webhookauth.ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := unix(tc.ts)
			err := verify(t, v, hdr("X-Webhook-Signature", signedAt(ts, "b"), "X-Relay-Timestamp", ts), "b")
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

func TestTimestampDefaultToleranceIsFiveMinutes(t *testing.T) {
	cfg := webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Header: "X-T"}}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
	for _, tc := range []struct {
		d    time.Duration
		want error
	}{
		{-5 * time.Minute, nil}, {5 * time.Minute, nil},
		{-5*time.Minute - time.Second, webhookauth.ErrStale}, {5*time.Minute + time.Second, webhookauth.ErrStale},
	} {
		ts := unix(now.Add(tc.d))
		err := verify(t, v, hdr("X-Webhook-Signature", signedAt(ts, "b"), "X-T", ts), "b")
		if tc.want == nil && err != nil || tc.want != nil && !isErr(err, tc.want) {
			t.Errorf("skew %v: %v, want %v", tc.d, err, tc.want)
		}
	}
}

func TestTimestampFormats(t *testing.T) {
	type tc struct {
		name   string
		format webhookauth.TimestampFormat
		ts     string
		want   error
	}
	sec := unix(now)
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	rfc := now.Format(time.RFC3339)
	for _, c := range []tc{
		{"unix default", "", sec, nil},
		{"unix", webhookauth.TimestampUnix, sec, nil},
		{"unix given ms is far in the future, not guessed", webhookauth.TimestampUnix, ms, webhookauth.ErrStale},
		{"unix_ms", webhookauth.TimestampUnixMilli, ms, nil},
		{"unix_ms given seconds is long ago", webhookauth.TimestampUnixMilli, sec, webhookauth.ErrStale},
		{"rfc3339", webhookauth.TimestampRFC3339, rfc, nil},
		{"rfc3339 with offset", webhookauth.TimestampRFC3339, now.In(time.FixedZone("x", 2*3600)).Format(time.RFC3339), nil},
		{"rfc3339 fractional", webhookauth.TimestampRFC3339, now.Format("2006-01-02T15:04:05.000Z07:00"), nil},
		{"rfc3339 given unix", webhookauth.TimestampRFC3339, sec, webhookauth.ErrMalformed},
		{"unix given rfc3339", webhookauth.TimestampUnix, rfc, webhookauth.ErrMalformed},
		{"either: unix", webhookauth.TimestampUnixOrRFC3339, sec, nil},
		{"either: rfc3339", webhookauth.TimestampUnixOrRFC3339, rfc, nil},
		{"either: junk", webhookauth.TimestampUnixOrRFC3339, "tomorrow", webhookauth.ErrMalformed},
		{"unix: sign", webhookauth.TimestampUnix, "+" + sec, webhookauth.ErrMalformed},
		{"unix: negative", webhookauth.TimestampUnix, "-1", webhookauth.ErrMalformed},
		{"unix: float", webhookauth.TimestampUnix, sec + ".5", webhookauth.ErrMalformed},
		{"unix: exponent", webhookauth.TimestampUnix, "1e9", webhookauth.ErrMalformed},
		{"unix: hex", webhookauth.TimestampUnix, "0x10", webhookauth.ErrMalformed},
		{"unix: inner space", webhookauth.TimestampUnix, sec[:3] + " " + sec[3:], webhookauth.ErrMalformed},
		{"unix: overflow", webhookauth.TimestampUnix, strings.Repeat("9", 30), webhookauth.ErrMalformed},
		{"unix: leading zeros are another string", webhookauth.TimestampUnix, "000" + sec, webhookauth.ErrMalformed},
		{"rfc3339: date only", webhookauth.TimestampRFC3339, "2026-05-11", webhookauth.ErrMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := hmacWithTimestamp(webhookauth.Config{})
			cfg.Timestamp.Format = c.format
			v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
			err := verify(t, v, hdr("X-Webhook-Signature", signedAt(c.ts, "b"), "X-Relay-Timestamp", c.ts), "b")
			if c.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			wantErr(t, err, c.want)
		})
	}
}

func TestTimestampHeaderProblems(t *testing.T) {
	v := mustNew(t, hmacWithTimestamp(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{})
	ts := unix(now)
	sig := signedAt(ts, "b")
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig), "b"), webhookauth.ErrMissing)
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig, "X-Relay-Timestamp", ""), "b"), webhookauth.ErrMissing)
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig, "X-Relay-Timestamp", ts, "X-Relay-Timestamp", ts), "b"), webhookauth.ErrMalformed)
}

// The default layout signs the timestamp with the body, so a captured
// request cannot be given a fresh timestamp.
func TestTimestampIsSigned(t *testing.T) {
	v := mustNew(t, hmacWithTimestamp(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{})
	old := unix(now.Add(-time.Hour))
	captured := signedAt(old, "b")
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", captured, "X-Relay-Timestamp", unix(now)), "b"), webhookauth.ErrInvalid)
	// The replay of the capture as sent is stale, not accepted.
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", captured, "X-Relay-Timestamp", old), "b"), webhookauth.ErrStale)
	// A bad signature is reported as such even for a stale request.
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", strings.Repeat("00", 32), "X-Relay-Timestamp", old), "b"), webhookauth.ErrInvalid)
}

func TestTimestampLayouts(t *testing.T) {
	ts := unix(now)
	body := `{"x":1}`
	for _, tc := range []struct {
		name, payload, signed string
	}{
		{"default dot", "", ts + "." + body},
		{"slack style", "v0:{timestamp}:{body}", "v0:" + ts + ":" + body},
		{"concatenated", "{timestamp}{body}", ts + body},
		{"body first", "{body}|{timestamp}", body + "|" + ts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hmacWithTimestamp(webhookauth.Config{Payload: tc.payload})
			v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
			sig := macHex(sha256New, []byte(longSecret), []byte(tc.signed))
			res, err := v.Verify(context.Background(), webhookauth.Request{Header: hdr("X-Webhook-Signature", sig, "X-Relay-Timestamp", ts), Body: []byte(body)})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !res.Timestamp.Equal(now) {
				t.Errorf("Result.Timestamp = %v, want %v", res.Timestamp, now)
			}
		})
	}
}

func TestEd25519TimestampBeforeBody(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := webhookauth.Config{
		Scheme: webhookauth.SchemeEd25519, Header: "X-Relay-Signature", Payload: "{timestamp}{body}",
		Timestamp: webhookauth.Timestamp{Header: "X-Relay-Timestamp", Format: webhookauth.TimestampUnixOrRFC3339},
	}
	v := mustNew(t, cfg, secrets(string(pub)), webhookauth.Deps{})
	for _, ts := range []string{unix(now), now.Format(time.RFC3339)} {
		sig := hex.EncodeToString(ed25519.Sign(priv, []byte(ts+`{"type":1}`)))
		if err := verify(t, v, hdr("X-Relay-Signature", sig, "X-Relay-Timestamp", ts), `{"type":1}`); err != nil {
			t.Errorf("ts %q: %v", ts, err)
		}
	}
	// With nothing between timestamp and body, the signature of "<ts>123"
	// also covers the header "<ts>1" with body "23". The shifted digit moves
	// the instant by an order of magnitude, so the window refuses it.
	sig := hex.EncodeToString(ed25519.Sign(priv, []byte(unix(now)+"123")))
	if err := verify(t, v, hdr("X-Relay-Signature", sig, "X-Relay-Timestamp", unix(now)), "123"); err != nil {
		t.Fatalf("unshifted: %v", err)
	}
	wantErr(t, verify(t, v, hdr("X-Relay-Signature", sig, "X-Relay-Timestamp", unix(now)+"1"), "23"), webhookauth.ErrStale)
}

func TestUnsignedTimestampNeedsConsent(t *testing.T) {
	src := secrets(longSecret)
	// A timestamp the signature does not cover can be refreshed by anyone
	// holding a captured request, so it is only accepted when asked for.
	cfg := webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Header: "X-T"}, Payload: "{body}"}
	_, err := webhookauth.New(cfg, src, webhookauth.Deps{})
	wantErr(t, err, webhookauth.ErrConfig)
	cfg.Timestamp.AllowUnsigned = true
	v := mustNew(t, cfg, src, webhookauth.Deps{})
	ts := unix(now)
	if err := verify(t, v, hdr("X-Webhook-Signature", macHex(sha256New, []byte(longSecret), []byte("b")), "X-T", ts), "b"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Bearer and token authenticate no payload at all.
	for _, s := range []webhookauth.Scheme{webhookauth.SchemeBearer, webhookauth.SchemeToken} {
		cfg := webhookauth.Config{Scheme: s, Timestamp: webhookauth.Timestamp{Header: "X-T"}}
		_, err := webhookauth.New(cfg, src, webhookauth.Deps{})
		wantErr(t, err, webhookauth.ErrConfig)
		cfg.Timestamp.AllowUnsigned = true
		v := mustNew(t, cfg, src, webhookauth.Deps{})
		h := hdr("X-T", ts, "Authorization", "Bearer "+longSecret, "X-Webhook-Token", longSecret)
		if err := verify(t, v, h, ""); err != nil {
			t.Errorf("%s: %v", s, err)
		}
		h.Set("X-T", unix(now.Add(-time.Hour)))
		wantErr(t, verify(t, v, h, ""), webhookauth.ErrStale)
	}
}

func TestTimestampConfigErrors(t *testing.T) {
	src := secrets(longSecret)
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
	}{
		{"format without header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Format: webhookauth.TimestampUnix}}},
		{"tolerance without header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Tolerance: time.Minute}}},
		{"unknown format", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Header: "X-T", Format: "iso"}}},
		{"negative tolerance", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Header: "X-T", Tolerance: -time.Second}}},
		{"tolerance over a day", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Header: "X-T", Tolerance: 25 * time.Hour}}},
		{"bad header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Timestamp: webhookauth.Timestamp{Header: "X T"}}},
		{"placeholder without header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Payload: "{timestamp}.{body}"}},
		{"same header as signature", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Header: "x-t", Timestamp: webhookauth.Timestamp{Header: "X-T"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhookauth.New(tc.cfg, src, webhookauth.Deps{})
			wantErr(t, err, webhookauth.ErrConfig)
		})
	}
}

func TestSystemClockWhenNoneInjected(t *testing.T) {
	cfg := hmacWithTimestamp(webhookauth.Config{})
	v, err := webhookauth.New(cfg, secrets(longSecret), webhookauth.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	ts := unix(time.Now())
	if err := verify(t, v, hdr("X-Webhook-Signature", signedAt(ts, "b"), "X-Relay-Timestamp", ts), "b"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func isErr(got, want error) bool { return errors.Is(got, want) }
