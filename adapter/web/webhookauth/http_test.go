package webhookauth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
)

func post(body string, kv ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	for i := 0; i < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}
	return r
}

func TestVerifyHTTPReturnsRawBodyAndRestoresIt(t *testing.T) {
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	body := `{"b" : 1,  "a": 2}`
	r := post(body, "X-Webhook-Signature", sigOf(body))
	got, res, err := v.VerifyHTTP(r)
	if err != nil || res == nil {
		t.Fatalf("VerifyHTTP: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want the bytes as received", got)
	}
	// The handler downstream reads the same bytes again.
	again, err := io.ReadAll(r.Body)
	if err != nil || string(again) != body {
		t.Fatalf("body after verification = %q, %v", again, err)
	}
}

func TestVerifyHTTPBodyLimit(t *testing.T) {
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, MaxBodyBytes: 8}, secrets(longSecret), webhookauth.Deps{})
	for _, tc := range []struct {
		name string
		r    func() *http.Request
		want error
	}{
		{"at the limit", func() *http.Request { return post("12345678", "X-Webhook-Signature", sigOf("12345678")) }, nil},
		{"over by one", func() *http.Request { return post("123456789", "X-Webhook-Signature", sigOf("123456789")) }, webhookauth.ErrTooLarge},
		{"over, length unknown", func() *http.Request {
			r := post("123456789", "X-Webhook-Signature", sigOf("123456789"))
			r.ContentLength = -1
			r.Body = io.NopCloser(io.MultiReader(strings.NewReader("12345"), strings.NewReader("6789")))
			return r
		}, webhookauth.ErrTooLarge},
		{"host's own limit trips first", func() *http.Request {
			r := post(strings.Repeat("x", 100), "X-Webhook-Signature", sigOf("x"))
			r.ContentLength = -1
			r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 4)
			return r
		}, webhookauth.ErrTooLarge},
		{"unreadable", func() *http.Request {
			r := post("", "X-Webhook-Signature", sigOf(""))
			r.Body = io.NopCloser(&failReader{})
			return r
		}, webhookauth.ErrMalformed},
		{"no body at all", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/hook", nil)
			r.Body = nil
			r.Header.Set("X-Webhook-Signature", sigOf(""))
			return r
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := v.VerifyHTTP(tc.r())
			if tc.want == nil {
				if err != nil {
					t.Fatalf("VerifyHTTP: %v", err)
				}
				return
			}
			wantErr(t, err, tc.want)
		})
	}
	// The default limit is 1 MiB.
	d := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	big := strings.Repeat("a", 1<<20+1)
	_, _, err := d.VerifyHTTP(post(big, "X-Webhook-Signature", sigOf(big)))
	wantErr(t, err, webhookauth.ErrTooLarge)
}

type failReader struct{}

func (*failReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// An oversize body is refused before any secret is resolved.
func TestVerifyHTTPDoesNotResolveSecretsForOversizeBody(t *testing.T) {
	calls := 0
	src := webhookauth.SecretFunc(func(context.Context) ([][]byte, error) { calls++; return [][]byte{[]byte(longSecret)}, nil })
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, MaxBodyBytes: 4}, src, webhookauth.Deps{})
	_, _, _ = v.VerifyHTTP(post("too long", "X-Webhook-Signature", sigOf("too long")))
	if calls != 0 {
		t.Fatalf("secret source called %d times", calls)
	}
}

// A receiver wired the way an adapter would: verify, answer on failure,
// release the delivery when the host cannot take it.
func TestReceiverEndToEnd(t *testing.T) {
	store := webhookauth.NewMemoryReplayStore(0, clock{now})
	cfg := webhookauth.Config{
		Scheme: webhookauth.SchemeHMAC, Prefix: "sha256=",
		Timestamp: webhookauth.Timestamp{Header: "X-Relay-Timestamp"},
		Replay:    webhookauth.Replay{Source: webhookauth.ReplayBySignature},
	}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{Replay: store, Scope: "relay-1"})
	accept := false
	var delivered []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, res, err := v.VerifyHTTP(r)
		if err != nil {
			v.Reject(w, err)
			return
		}
		if !accept {
			_ = res.Release(r.Context())
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		delivered = append(delivered, string(body))
		w.WriteHeader(http.StatusNoContent)
	})
	ts := unix(now)
	send := func(sig string) int {
		r := post("hello", "X-Webhook-Signature", sig, "X-Relay-Timestamp", ts)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	good := "sha256=" + signedAt(ts, "hello")
	if got := send("sha256=" + strings.Repeat("0", 64)); got != 401 {
		t.Errorf("forged: %d", got)
	}
	if got := send(good); got != 503 { // host busy: sender retries
		t.Errorf("busy host: %d", got)
	}
	accept = true
	if got := send(good); got != 204 { // the retry is not a replay
		t.Errorf("retry: %d", got)
	}
	if got := send(good); got != 409 {
		t.Errorf("duplicate: %d", got)
	}
	if len(delivered) != 1 {
		t.Errorf("delivered %v", delivered)
	}
}

// Senders already in the field keep their header names, encodings and
// signed layout; nothing here needs a code change, only configuration.
func TestLegacySenderConventionsByConfigAlone(t *testing.T) {
	short := "secret" // legacy senders may use short secrets: lowering the floor is explicit
	ts := unix(now)
	t.Run("token header", func(t *testing.T) {
		cfg := webhookauth.Config{Scheme: webhookauth.SchemeToken, Header: "X-Relay-Token", MinSecretBytes: 4}
		v := mustNew(t, cfg, secrets(short), webhookauth.Deps{})
		if err := verify(t, v, hdr("X-Relay-Token", short), ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("hmac over the body, optional prefix, unsigned timestamp and delivery id", func(t *testing.T) {
		store := webhookauth.NewMemoryReplayStore(0, clock{now})
		cfg := webhookauth.Config{
			Scheme: webhookauth.SchemeHMAC, Header: "X-Relay-Signature",
			Prefix: "sha256=", PrefixOptional: true, MinSecretBytes: 4, Payload: "{body}",
			Timestamp: webhookauth.Timestamp{Header: "X-Relay-Timestamp", Format: webhookauth.TimestampUnixOrRFC3339, AllowUnsigned: true},
			Replay:    webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Relay-Delivery-Id", AllowUnsigned: true},
		}
		v := mustNew(t, cfg, secrets(short), webhookauth.Deps{Replay: store})
		body := `{"ok":true}`
		sig := macHex(sha256New, []byte(short), []byte(body))
		for i, form := range []string{"sha256=" + sig, sig} {
			stamp := []string{ts, now.Format(time.RFC3339)}[i]
			h := hdr("X-Relay-Signature", form, "X-Relay-Timestamp", stamp, "X-Relay-Delivery-Id", "d"+stamp)
			if err := verify(t, v, h, body); err != nil {
				t.Fatalf("form %d: %v", i, err)
			}
		}
		h := hdr("X-Relay-Signature", sig, "X-Relay-Timestamp", ts, "X-Relay-Delivery-Id", "d"+ts)
		wantErr(t, verify(t, v, h, body), webhookauth.ErrReplay)
	})
	t.Run("ed25519 over timestamp then body, hex key", func(t *testing.T) {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		cfg := webhookauth.Config{
			Scheme: webhookauth.SchemeEd25519, Header: "X-Signature-Ed25519", KeyEncoding: webhookauth.EncodingHex,
			Payload:   "{timestamp}{body}",
			Timestamp: webhookauth.Timestamp{Header: "X-Signature-Timestamp", Format: webhookauth.TimestampUnixOrRFC3339},
		}
		v := mustNew(t, cfg, secrets(hex.EncodeToString(pub)), webhookauth.Deps{})
		body := `{"type":1}`
		sig := hex.EncodeToString(ed25519.Sign(priv, []byte(ts+body)))
		if err := verify(t, v, hdr("X-Signature-Ed25519", sig, "X-Signature-Timestamp", ts), body); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bearer in a custom header", func(t *testing.T) {
		cfg := webhookauth.Config{Scheme: webhookauth.SchemeBearer, Header: "X-Relay-Auth", MinSecretBytes: 4}
		v := mustNew(t, cfg, secrets(short), webhookauth.Deps{})
		if err := verify(t, v, hdr("X-Relay-Auth", "Bearer "+short), ""); err != nil {
			t.Fatal(err)
		}
	})
}
