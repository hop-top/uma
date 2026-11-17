package httpmail_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const minimalJSON = `{"from":"alice@peers.test","to":"support@example.com","subject":"hi","body":"hello"}`

func TestMethodMustBePOST(t *testing.T) {
	r := newRig(t, baseConfig())
	req, _ := http.NewRequest(http.MethodGet, "/hooks/mail", nil)
	rec := recorder(r.h, req)
	wantStatus(t, rec, http.StatusMethodNotAllowed)
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q", got)
	}
}

func TestRequestWithoutCredentialIsRefusedBeforeAnythingIsRead(t *testing.T) {
	r := newRig(t, baseConfig())
	// A body no decoder accepts and a content type no format takes: a 401
	// proves authentication ran first.
	rec := r.do(request{contentType: "text/plain", body: "{{{", noAuth: true})
	wantStatus(t, rec, http.StatusUnauthorized)
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if n := len(r.sink.ingresses()); n != 0 {
		t.Fatalf("sink saw %d ingresses", n)
	}
}

func TestWrongTokenIsRefused(t *testing.T) {
	r := newRig(t, baseConfig())
	rec := r.do(request{contentType: "application/json", body: minimalJSON, header: http.Header{"Authorization": {"Bearer not-the-token-0123456789"}}})
	wantStatus(t, rec, http.StatusUnauthorized)
	if n := len(r.sink.ingresses()); n != 0 {
		t.Fatalf("sink saw %d ingresses", n)
	}
}

func TestCredentialIsResolvedPerRequestForThePurpose(t *testing.T) {
	r := newRig(t, baseConfig())
	for range 2 {
		r.do(request{contentType: "text/plain", body: "x"})
	}
	reqs := r.creds.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d credential resolutions, want one per request", len(reqs))
	}
	got := reqs[0]
	if got.Channel != chRef || got.Reference != "relay-cred" || got.Purpose != httpmail.PurposeWebhook {
		t.Errorf("request %+v", got)
	}
}

func TestCredentialFailureIsUnavailable(t *testing.T) {
	r := newRig(t, baseConfig())
	r.creds.err = errors.New("vault down")
	wantStatus(t, r.postJSON(minimalJSON), http.StatusServiceUnavailable)
}

func TestUnusableSecretFailsClosed(t *testing.T) {
	r := newRig(t, baseConfig())
	r.creds.cred = ports.Secret{Bytes: []byte("short")}
	wantStatus(t, r.postJSON(minimalJSON), http.StatusServiceUnavailable)
	if n := len(r.sink.ingresses()); n != 0 {
		t.Fatalf("sink saw %d ingresses", n)
	}
}

func TestUnknownContentTypeIsUnsupported(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.do(request{contentType: "text/plain", body: "hello"}), http.StatusUnsupportedMediaType)
	wantStatus(t, r.do(request{body: "hello"}), http.StatusUnsupportedMediaType)
}

// A sender that signs the body the way a hosted service does: HMAC-SHA-256
// of the timestamp header followed by the body, hex.
func TestSignedSenderWithTimestamp(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = httpmail.Auth{
		Scheme: "hmac", Header: "X-Relay-Signature", Payload: "{timestamp}{body}",
		Timestamp: &httpmail.AuthTimestamp{Header: "X-Relay-Timestamp"},
	}
	r := newRig(t, cfg)
	sign := func(ts, body string) http.Header {
		m := hmac.New(sha256.New, []byte(token))
		m.Write([]byte(ts + body))
		return http.Header{"X-Relay-Timestamp": {ts}, "X-Relay-Signature": {hex.EncodeToString(m.Sum(nil))}}
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	rq := request{contentType: "application/json", body: minimalJSON, noAuth: true}

	rq.header = sign(ts, minimalJSON)
	if rec := r.do(rq); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusBadRequest {
		t.Fatalf("a correct signature is refused: %d", rec.Code)
	}
	rq.header = sign(ts, minimalJSON+" ")
	wantStatus(t, r.do(rq), http.StatusUnauthorized)
	stale := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	rq.header = sign(stale, minimalJSON)
	wantStatus(t, r.do(rq), http.StatusUnauthorized)
}

func TestReplayedDeliveryIsRefusedAndReleasedWhenNotProcessed(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = httpmail.Auth{
		Scheme: "hmac", Header: "X-Relay-Signature",
		Replay: &httpmail.AuthReplay{Source: "signature"},
	}
	store := webhookauth.NewMemoryReplayStore(16, fixedClock{now})
	r := newRig(t, cfg, func(o *httpmail.Options) { o.ReplayStore = store })
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(minimalJSON))
	hdr := http.Header{"X-Relay-Signature": {hex.EncodeToString(m.Sum(nil))}}
	rq := request{contentType: "application/json", body: minimalJSON, noAuth: true, header: hdr}

	r.sink.fail(&model.Error{Code: model.Unavailable, SafeMessage: "down"})
	wantStatus(t, r.do(rq), http.StatusServiceUnavailable)
	// The sender retries: the first attempt was not processed, so this is
	// no replay.
	r.sink.fail(nil)
	wantStatus(t, r.do(rq), http.StatusOK)
	// Now it was.
	wantStatus(t, r.do(rq), http.StatusConflict)
}

func TestClosedInstanceIsUnavailable(t *testing.T) {
	r := newRig(t, baseConfig())
	_ = r.inst.Close(t.Context())
	wantStatus(t, r.postJSON(minimalJSON), http.StatusServiceUnavailable)
}

func TestCapabilitiesNeedACredentialPort(t *testing.T) {
	f := httpmail.NewFactory(httpmail.Options{})
	for _, tc := range []struct {
		name  string
		creds ports.Credentials
		want  bool
	}{{"with port", newCredHost(), true}, {"without", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := f.Open(t.Context(), channelOf(baseConfig()), adapter.Dependencies{Credentials: tc.creds})
			if err != nil {
				t.Fatal(err)
			}
			st, err := inst.Capabilities(t.Context())
			if err != nil || len(st) != 1 || st[0].Name != model.CapabilityReceiveHTTP || st[0].Available != tc.want {
				t.Fatalf("Capabilities = %+v, %v", st, err)
			}
			if !tc.want && st[0].Reason != model.Unavailable {
				t.Errorf("reason %q", st[0].Reason)
			}
		})
	}
}
