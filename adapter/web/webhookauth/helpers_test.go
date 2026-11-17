package webhookauth_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
)

// now is the fixed instant every test verifies at.
var now = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

// clock is a fixed ports.Clock.
type clock struct{ t time.Time }

func (c clock) Now() time.Time { return c.t }

// secrets is a SecretSource returning keys, as a host's credential port
// would.
func secrets(keys ...string) webhookauth.SecretSource {
	return webhookauth.SecretFunc(func(context.Context) ([][]byte, error) {
		out := make([][]byte, len(keys))
		for i, k := range keys {
			out[i] = []byte(k)
		}
		return out, nil
	})
}

// failing is a SecretSource whose host cannot resolve the credential.
func failing(err error) webhookauth.SecretSource {
	return webhookauth.SecretFunc(func(context.Context) ([][]byte, error) { return nil, err })
}

func mustNew(t *testing.T, cfg webhookauth.Config, src webhookauth.SecretSource, deps webhookauth.Deps) *webhookauth.Verifier {
	t.Helper()
	if deps.Clock == nil {
		deps.Clock = clock{now}
	}
	v, err := webhookauth.New(cfg, src, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func verify(t *testing.T, v *webhookauth.Verifier, h http.Header, body string) error {
	t.Helper()
	_, err := v.Verify(context.Background(), webhookauth.Request{Header: h, Body: []byte(body)})
	return err
}

func wantErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

const longSecret = "0123456789abcdef0123456789abcdef"

var errStoreDown = errors.New("credential store unreachable")
