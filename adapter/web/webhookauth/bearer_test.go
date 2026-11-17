package webhookauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
)

func TestBearer(t *testing.T) {
	cfg := webhookauth.Config{Scheme: webhookauth.SchemeBearer}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
	for _, tc := range []struct {
		name string
		h    http.Header
		want error
	}{
		{"valid", hdr("Authorization", "Bearer "+longSecret), nil},
		// RFC 9110 11.1: the auth scheme is case-insensitive.
		{"scheme lowercase", hdr("Authorization", "bearer "+longSecret), nil},
		{"scheme uppercase", hdr("Authorization", "BEARER "+longSecret), nil},
		// RFC 9110 11.6.2: one or more spaces separate scheme and credentials.
		{"extra spaces", hdr("Authorization", "Bearer   "+longSecret), nil},
		{"absent", hdr(), webhookauth.ErrMissing},
		{"other scheme", hdr("Authorization", "Basic "+longSecret), webhookauth.ErrMissing},
		{"scheme alone", hdr("Authorization", "Bearer"), webhookauth.ErrMalformed},
		{"empty token", hdr("Authorization", "Bearer  "), webhookauth.ErrMalformed},
		{"token with space", hdr("Authorization", "Bearer "+longSecret+" x"), webhookauth.ErrMalformed},
		{"wrong token", hdr("Authorization", "Bearer "+strings.ToUpper(longSecret)), webhookauth.ErrInvalid},
		{"prefix of secret", hdr("Authorization", "Bearer "+longSecret[:31]), webhookauth.ErrInvalid},
		{"secret plus suffix", hdr("Authorization", "Bearer "+longSecret+"x"), webhookauth.ErrInvalid},
		{"bare secret without scheme", hdr("Authorization", longSecret), webhookauth.ErrMissing},
		// A repeated header is ambiguous: a proxy and the origin may pick
		// different values.
		{"duplicate headers", hdr("Authorization", "Bearer "+longSecret, "Authorization", "Bearer "+longSecret), webhookauth.ErrMalformed},
		{"duplicate with wrong first", hdr("Authorization", "Bearer nope", "Authorization", "Bearer "+longSecret), webhookauth.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verify(t, v, tc.h, "{}")
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

func TestBearerCustomHeaderAnyCase(t *testing.T) {
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeBearer, Header: "x-relay-auth"}, secrets(longSecret), webhookauth.Deps{})
	// Header maps built by hand need not use canonical keys; lookup is
	// case-insensitive on both sides.
	for _, key := range []string{"X-Relay-Auth", "x-relay-auth", "X-RELAY-AUTH"} {
		h := http.Header{key: {"Bearer " + longSecret}}
		if err := verify(t, v, h, ""); err != nil {
			t.Errorf("key %q: %v", key, err)
		}
	}
	// The same header under two spellings is still a repeated header.
	h := http.Header{"X-Relay-Auth": {"Bearer " + longSecret}, "x-relay-auth": {"Bearer " + longSecret}}
	wantErr(t, verify(t, v, h, ""), webhookauth.ErrMalformed)
	// The default header is not consulted once another is configured.
	wantErr(t, verify(t, v, hdr("Authorization", "Bearer "+longSecret), ""), webhookauth.ErrMissing)
}

func TestToken(t *testing.T) {
	cfg := webhookauth.Config{Scheme: webhookauth.SchemeToken, Header: "X-Relay-Token"}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
	for _, tc := range []struct {
		name string
		h    http.Header
		want error
	}{
		{"valid", hdr("X-Relay-Token", longSecret), nil},
		{"absent", hdr(), webhookauth.ErrMissing},
		{"empty", hdr("X-Relay-Token", ""), webhookauth.ErrMissing},
		{"wrong", hdr("X-Relay-Token", longSecret[:31]+"x"), webhookauth.ErrInvalid},
		{"scheme word is not stripped", hdr("X-Relay-Token", "Bearer "+longSecret), webhookauth.ErrInvalid},
		{"duplicate", hdr("X-Relay-Token", longSecret, "X-Relay-Token", longSecret), webhookauth.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verify(t, v, tc.h, "")
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

func TestTokenDefaultHeaderIsNeutral(t *testing.T) {
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeToken}, secrets(longSecret), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Token", longSecret), ""); err != nil {
		t.Fatalf("default header: %v", err)
	}
}

// Fail closed: nothing but a usable secret ever authenticates.
func TestSecretFailsClosed(t *testing.T) {
	good := hdr("Authorization", "Bearer "+longSecret)
	for _, tc := range []struct {
		name string
		src  webhookauth.SecretSource
		want error
	}{
		{"no secrets", secrets(), webhookauth.ErrSecret},
		{"empty secret", secrets(""), webhookauth.ErrSecret},
		{"empty secret and empty token", secrets(""), webhookauth.ErrSecret},
		{"too short", secrets("short"), webhookauth.ErrSecret},
		{"one bad of two", secrets(longSecret, ""), webhookauth.ErrSecret},
		{"source error", failing(errors.New("vault down")), webhookauth.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeBearer}, tc.src, webhookauth.Deps{})
			wantErr(t, verify(t, v, good, ""), tc.want)
			// An empty presented credential never matches an empty secret.
			wantErr(t, verify(t, v, hdr("Authorization", "Bearer "), ""), webhookauth.ErrMalformed)
		})
	}
}

func TestSecretErrorDoesNotLeakSecret(t *testing.T) {
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeBearer}, secrets(longSecret), webhookauth.Deps{})
	err := verify(t, v, hdr("Authorization", "Bearer "+strings.ToUpper(longSecret)), "")
	if err == nil || strings.Contains(err.Error(), longSecret) || strings.Contains(strings.ToLower(err.Error()), strings.ToLower(longSecret)) {
		t.Fatalf("error %v leaks content", err)
	}
}

func TestMinSecretBytesIsConfigurable(t *testing.T) {
	cfg := webhookauth.Config{Scheme: webhookauth.SchemeToken, MinSecretBytes: 4}
	v := mustNew(t, cfg, secrets("Jefe"), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Token", "Jefe"), ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// An empty secret stays refused whatever the minimum.
	v = mustNew(t, cfg, secrets(""), webhookauth.Deps{})
	wantErr(t, verify(t, v, hdr("X-Webhook-Token", "x"), ""), webhookauth.ErrSecret)
	wantErr(t, verify(t, v, hdr("X-Webhook-Token", ""), ""), webhookauth.ErrMissing)
}

func TestSecretRotationAcceptsEitherKey(t *testing.T) {
	prev := strings.Repeat("p", 32)
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeBearer}, secrets(longSecret, prev), webhookauth.Deps{})
	for _, s := range []string{longSecret, prev} {
		if err := verify(t, v, hdr("Authorization", "Bearer "+s), ""); err != nil {
			t.Errorf("secret %q.. rejected: %v", s[:3], err)
		}
	}
}

func TestStatusAndReject(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{webhookauth.ErrMissing, http.StatusUnauthorized},
		{webhookauth.ErrInvalid, http.StatusUnauthorized},
		{webhookauth.ErrMalformed, http.StatusBadRequest},
		{webhookauth.ErrTooLarge, http.StatusRequestEntityTooLarge},
		{webhookauth.ErrSecret, http.StatusServiceUnavailable},
		{webhookauth.ErrUnavailable, http.StatusServiceUnavailable},
		{webhookauth.ErrStale, http.StatusUnauthorized},
		{errors.New("other"), http.StatusInternalServerError},
		{nil, http.StatusOK},
	} {
		if got := webhookauth.Status(tc.err); got != tc.status {
			t.Errorf("Status(%v) = %d, want %d", tc.err, got, tc.status)
		}
	}
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeBearer}, secrets(longSecret), webhookauth.Deps{})
	// RFC 6750 3: no credentials, no error code; a bad token is invalid_token.
	rec := httptest.NewRecorder()
	v.Reject(rec, webhookauth.ErrMissing)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("missing: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	rec = httptest.NewRecorder()
	v.Reject(rec, webhookauth.ErrInvalid)
	if got := rec.Header().Get("WWW-Authenticate"); rec.Code != 401 || got != `Bearer error="invalid_token"` {
		t.Errorf("invalid: %d %q", rec.Code, got)
	}
	rec = httptest.NewRecorder()
	v.Reject(rec, webhookauth.ErrMalformed)
	if got := rec.Header().Get("WWW-Authenticate"); rec.Code != 400 || got != `Bearer error="invalid_request"` {
		t.Errorf("malformed: %d %q", rec.Code, got)
	}
	tok := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeToken}, secrets(longSecret), webhookauth.Deps{})
	rec = httptest.NewRecorder()
	tok.Reject(rec, webhookauth.ErrInvalid)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("token scheme sends a bearer challenge: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestEqualHidesLength(t *testing.T) {
	var lens []int
	restore := webhookauth.SpyEqual(func(a, b []byte) { lens = append(lens, len(a), len(b)) })
	defer restore()
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeBearer}, secrets(longSecret, strings.Repeat("q", 40)), webhookauth.Deps{})
	for _, presented := range []string{"x", longSecret, longSecret + strings.Repeat("y", 100)} {
		lens = lens[:0]
		_ = verify(t, v, hdr("Authorization", "Bearer "+presented), "")
		// Every key is compared, none early, and always as fixed-size
		// digests so a length mismatch costs the same as a match.
		if len(lens) != 4 {
			t.Fatalf("presented %d bytes: %d comparisons, want one per key", len(presented), len(lens)/2)
		}
		for _, n := range lens {
			if n != 32 {
				t.Fatalf("presented %d bytes: compared %d-byte values, want 32-byte digests", len(presented), n)
			}
		}
	}
}

var _ = context.Background
