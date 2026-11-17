package botauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"hop.top/uma/model"
)

const (
	appID      = "00000000-0000-4000-8000-0000000000a1"
	tenantID   = "00000000-0000-4000-8000-0000000000e1"
	testSecret = "fixture-secret-not-real" // #nosec G101 -- fixture value
)

var t0 = time.Unix(1_700_000_000, 0).UTC()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// endpoint is a stand-in for the Microsoft Entra token endpoint, modelled
// on
// https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow#second-case-access-token-request-with-a-certificate
// and the Bot Connector token request in
// https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication.
// It records each request and answers a numbered token.
type endpoint struct {
	mu        sync.Mutex
	forms     []url.Values
	paths     []string
	n         atomic.Int32
	expiresIn int
	status    int
	body      string
	header    http.Header
	delay     chan struct{}
}

func newEndpoint() *endpoint { return &endpoint{expiresIn: 3600} }

func (e *endpoint) Do(r *http.Request) (*http.Response, error) {
	if e.delay != nil {
		<-e.delay
	}
	_ = r.ParseForm()
	e.mu.Lock()
	e.forms = append(e.forms, r.PostForm)
	e.paths = append(e.paths, r.Method+" "+r.URL.String())
	status, body, hdr := e.status, e.body, e.header
	e.mu.Unlock()
	rec := httptest.NewRecorder()
	for k, v := range hdr {
		rec.Header()[k] = v
	}
	if status != 0 {
		rec.WriteHeader(status)
		_, _ = rec.WriteString(body)
		return rec.Result(), nil
	}
	n := e.n.Add(1)
	_ = json.NewEncoder(rec).Encode(map[string]any{
		"token_type": "Bearer", "expires_in": e.expiresIn, "ext_expires_in": e.expiresIn,
		"access_token": "tok-" + string(rune('0'+n)),
	})
	return rec.Result(), nil
}

func (e *endpoint) count() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.forms) }

func secretSource(t *testing.T, e *endpoint, c *clock, mod ...func(*Config)) *Source {
	t.Helper()
	cfg := Config{AppID: appID, Tenant: tenantID, Secret: testSecret, HTTP: e, Clock: c}
	for _, m := range mod {
		m(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecretRequestShape(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	tok, err := secretSource(t, e, c).Token(t.Context(), false)
	if err != nil || tok.Value != "tok-1" || !tok.Expiry.Equal(t0.Add(time.Hour)) {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	if want := "POST https://login.microsoftonline.com/" + tenantID + "/oauth2/v2.0/token"; e.paths[0] != want {
		t.Fatalf("request = %q, want %q", e.paths[0], want)
	}
	f := e.forms[0]
	if f.Get("grant_type") != "client_credentials" || f.Get("client_id") != appID ||
		f.Get("client_secret") != testSecret || f.Get("scope") != "https://api.botframework.com/.default" ||
		f.Get("client_assertion") != "" {
		t.Fatalf("form = %v", f)
	}
}

func TestMultiTenantAndAuthority(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	s := secretSource(t, e, c, func(cf *Config) {
		cf.Tenant, cf.Authority, cf.Scope = "", "https://login.microsoftonline.us/", "https://api.botframework.us/.default"
	})
	if _, err := s.Token(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if want := "POST https://login.microsoftonline.us/botframework.com/oauth2/v2.0/token"; e.paths[0] != want {
		t.Fatalf("request = %q", e.paths[0])
	}
	if e.forms[0].Get("scope") != "https://api.botframework.us/.default" {
		t.Fatalf("scope = %q", e.forms[0].Get("scope"))
	}
}

func TestCacheAndRefreshBeforeExpiry(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	s := secretSource(t, e, c)
	first, _ := s.Token(t.Context(), false)
	c.add(54 * time.Minute) // 6 min left: outside the 5 min skew
	if got, _ := s.Token(t.Context(), false); got != first || e.count() != 1 {
		t.Fatalf("cached token replaced early: %+v, %d mints", got, e.count())
	}
	c.add(2 * time.Minute) // 4 min left: inside the skew
	got, err := s.Token(t.Context(), false)
	if err != nil || got.Value != "tok-2" || e.count() != 2 {
		t.Fatalf("token = %+v, %v, mints %d", got, err, e.count())
	}
}

func TestShortLivedTokenRefreshesHalfWay(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	e.expiresIn = 120
	s := secretSource(t, e, c)
	_, _ = s.Token(t.Context(), false)
	c.add(30 * time.Second)
	_, _ = s.Token(t.Context(), false)
	if e.count() != 1 {
		t.Fatalf("minted at %d of 120s", e.count())
	}
	c.add(31 * time.Second)
	_, _ = s.Token(t.Context(), false)
	if e.count() != 2 {
		t.Fatal("not replaced past half its life")
	}
}

func TestFreshBypassesCacheExceptJustMinted(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	s := secretSource(t, e, c)
	_, _ = s.Token(t.Context(), false)
	if got, _ := s.Token(t.Context(), true); got.Value != "tok-1" || e.count() != 1 {
		t.Fatalf("fresh right after a mint minted again: %+v", got)
	}
	c.add(time.Minute)
	if got, _ := s.Token(t.Context(), true); got.Value != "tok-2" {
		t.Fatalf("fresh after the window = %+v", got)
	}
}

func TestConcurrentCallersShareOneMint(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	e.delay = make(chan struct{})
	s := secretSource(t, e, c)
	var wg sync.WaitGroup
	got := make([]Token, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = s.Token(t.Context(), false)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(e.delay)
	wg.Wait()
	if e.count() != 1 {
		t.Fatalf("%d mints for 16 callers", e.count())
	}
	for _, g := range got {
		if g.Value != "tok-1" {
			t.Fatalf("token = %+v", g)
		}
	}
}

func TestCallerCancelDoesNotAbortSharedMint(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	e.delay = make(chan struct{})
	s := secretSource(t, e, c)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { _, err := s.Token(ctx, false); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled caller got a token")
	}
	close(e.delay)
	if tok, err := s.Token(t.Context(), false); err != nil || tok.Value != "tok-1" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
}

func TestEndpointFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		hdr    http.Header
		code   model.ErrorCode
		has    string
	}{
		{"bad secret", 401, `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided. Trace ID: x " }`, nil, model.Unauthorized, "invalid_client AADSTS7000215"},
		{"bad request", 400, `{"error":"invalid_scope"}`, nil, model.Unauthorized, "invalid_scope"},
		{"throttled", 429, `{}`, http.Header{"Retry-After": {"7"}}, model.RateLimited, ""},
		{"outage", 503, `<html>`, nil, model.Unavailable, ""},
		{"garbage ok", 200, `not json`, nil, model.Transient, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, c := newEndpoint(), &clock{t: t0}
			e.status, e.body, e.header = tc.status, tc.body, tc.hdr
			_, err := secretSource(t, e, c).Token(t.Context(), false)
			me, ok := err.(*model.Error)
			if !ok || me.Code != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
			if !strings.Contains(me.SafeMessage, tc.has) || strings.Contains(me.SafeMessage, testSecret) || strings.Contains(me.SafeMessage, "Trace") {
				t.Fatalf("message = %q", me.SafeMessage)
			}
			if tc.status == 429 && me.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %v", me.RetryAfter)
			}
		})
	}
}

func TestFailureIsNotCached(t *testing.T) {
	e, c := newEndpoint(), &clock{t: t0}
	e.status = 503
	s := secretSource(t, e, c)
	_, _ = s.Token(t.Context(), false)
	e.mu.Lock()
	e.status = 0
	e.mu.Unlock()
	if tok, err := s.Token(t.Context(), false); err != nil || tok.Value == "" {
		t.Fatalf("after recovery: %+v, %v", tok, err)
	}
}

func TestConfigValidation(t *testing.T) {
	cert, key := testCert(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	e := newEndpoint()
	bad := map[string]Config{
		"no app":         {HTTP: e, Secret: "s"},
		"no http":        {AppID: appID, Secret: "s"},
		"no credential":  {AppID: appID, HTTP: e},
		"both":           {AppID: appID, HTTP: e, Secret: "s", Certificate: cert, Key: key},
		"cert only":      {AppID: appID, HTTP: e, Certificate: cert},
		"key only":       {AppID: appID, HTTP: e, Key: key},
		"mismatch":       {AppID: appID, HTTP: e, Certificate: cert, Key: other},
		"alg":            {AppID: appID, HTTP: e, Secret: "s", AssertionAlg: "HS256"},
		"http authority": {AppID: appID, HTTP: e, Secret: "s", Authority: "http://login.example.com"},
		"authority path": {AppID: appID, HTTP: e, Secret: "s", Authority: "https://login.example.com/x"},
		"authority user": {AppID: appID, HTTP: e, Secret: "s", Authority: "https://" + "u@login.example.com"},
	}
	for n, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s accepted", n)
		}
	}
	if _, err := New(Config{AppID: appID, HTTP: e, Secret: "s", Authority: "http://127.0.0.1:9"}); err != nil {
		t.Errorf("loopback http refused: %v", err)
	}
}

func testCert(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test bot"},
		NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(24 * time.Hour),
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test bot"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func TestCertificateAssertion(t *testing.T) {
	for _, alg := range []string{"", AlgPS256, AlgRS256} {
		t.Run("alg="+alg, func(t *testing.T) {
			cert, key := testCert(t)
			e, c := newEndpoint(), &clock{t: t0}
			s, err := New(Config{AppID: appID, Tenant: tenantID, Certificate: cert, Key: key, AssertionAlg: alg, HTTP: e, Clock: c})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Token(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			f := e.forms[0]
			if f.Get("client_secret") != "" || f.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" ||
				f.Get("grant_type") != "client_credentials" || f.Get("client_id") != appID {
				t.Fatalf("form = %v", f)
			}
			wantAlg := alg
			if wantAlg == "" {
				wantAlg = AlgPS256
			}
			claims := jwt.MapClaims{}
			tok, err := jwt.ParseWithClaims(f.Get("client_assertion"), claims, func(*jwt.Token) (any, error) {
				return &key.PublicKey, nil
			}, jwt.WithValidMethods([]string{wantAlg}), jwt.WithTimeFunc(c.Now),
				jwt.WithAudience("https://login.microsoftonline.com/"+tenantID+"/oauth2/v2.0/token"),
				jwt.WithIssuer(appID), jwt.WithSubject(appID), jwt.WithExpirationRequired())
			if err != nil || !tok.Valid {
				t.Fatalf("assertion does not verify: %v", err)
			}
			sum := sha256.Sum256(cert.Raw)
			if tok.Header["x5t#S256"] != base64.RawURLEncoding.EncodeToString(sum[:]) || tok.Header["typ"] != "JWT" {
				t.Fatalf("header = %v", tok.Header)
			}
			if _, has := tok.Header["x5t"]; has != (wantAlg == AlgRS256) {
				t.Fatalf("x5t present = %v for %s", has, wantAlg)
			}
			if claims["jti"] == "" || int64(claims["nbf"].(float64)) != t0.Unix() || int64(claims["exp"].(float64)) != t0.Add(10*time.Minute).Unix() {
				t.Fatalf("claims = %v", claims)
			}
		})
	}
}

func TestAssertionSignedByOtherKeyFailsVerification(t *testing.T) {
	cert, key := testCert(t)
	_, otherKey := testCert(t)
	e, c := newEndpoint(), &clock{t: t0}
	s, _ := New(Config{AppID: appID, Certificate: cert, Key: key, HTTP: e, Clock: c})
	_, _ = s.Token(t.Context(), false)
	_, err := jwt.Parse(e.forms[0].Get("client_assertion"), func(*jwt.Token) (any, error) { return &otherKey.PublicKey, nil })
	if err == nil {
		t.Fatal("verified against an unrelated key")
	}
}
