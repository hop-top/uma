// Package botauth mints and refreshes the Bot Connector access token of
// one Microsoft Teams bot, for hosts that run the microsoft.teams adapter.
//
// A bot registration is a Microsoft Entra application. The Bot Connector
// accepts a bearer token that application obtains from the token endpoint
// with the OAuth 2.0 client credentials grant, for the scope
// https://api.botframework.com/.default
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication).
// The endpoint is <authority>/<tenant>/oauth2/v2.0/token: a single-tenant
// bot uses its tenant ID, a multi-tenant bot the tenant botframework.com
// (the default here), and a sovereign cloud its own authority host
// (https://learn.microsoft.com/en-us/entra/identity-platform/authentication-national-cloud).
//
// The app proves itself with a client secret sent in the request body, or
// with a certificate: a client assertion, a JWT signed with the
// certificate's private key and sent as client_assertion with
// client_assertion_type urn:ietf:params:oauth:client-assertion-type:jwt-bearer
// (https://learn.microsoft.com/en-us/entra/identity-platform/certificate-credentials).
// The assertion's header carries the certificate's x5t#S256 thumbprint
// (base64url SHA-256 of its DER); its claims are aud (the token endpoint),
// iss and sub (the app ID), jti, nbf, iat and a short exp.
//
// A Source keeps the token in memory, mints the next one before the cached
// one expires, and shares one mint among concurrent callers. Its HTTP
// client and clock are injected. It never logs or returns a secret, key,
// assertion or token.
//
// Use it with the adapter through teams.Options.Tokens.
package botauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- x5t is a certificate identifier, not a security hash
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const (
	// DefaultScope is what the Bot Connector's token is requested for.
	DefaultScope = "https://api.botframework.com/.default"
	// DefaultAuthority is the public cloud's Microsoft Entra endpoint.
	DefaultAuthority = "https://login.microsoftonline.com"
	// MultiTenant is the tenant a multi-tenant bot's token is requested from.
	MultiTenant = "botframework.com"
	// DefaultSkew is how long before expiry a token is replaced.
	DefaultSkew = 5 * time.Minute

	// freshWindow is how long a token just minted answers a request for a
	// fresh one: minting again would not cure a refusal of a new token.
	freshWindow = 30 * time.Second
	mintTimeout = 30 * time.Second
	assertTTL   = 10 * time.Minute
	maxBody     = 1 << 20

	assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// Assertion signing algorithms. PS256 is what the Entra documentation
// specifies; RS256 (with the SHA-1 x5t header as well) is the older form
// the endpoint still accepts.
const (
	AlgPS256 = "PS256"
	AlgRS256 = "RS256"
)

// Token is a minted Bot Connector access token.
type Token struct {
	Value  string
	Expiry time.Time
}

// Config configures a Source. Exactly one of Secret and Certificate+Key.
type Config struct {
	// AppID is the bot's Microsoft Entra application (client) ID. Required.
	AppID string
	// Tenant is the tenant ID of a single-tenant bot; empty is MultiTenant.
	Tenant string
	// Authority is the token endpoint's origin; empty is DefaultAuthority.
	// It is a host setting: the credential is sent there, so it must be
	// https (http only for a loopback host, a stand-in endpoint).
	Authority string
	// Scope is the token's scope; empty is DefaultScope.
	Scope string

	// Secret is the app's client secret value.
	Secret string
	// Certificate is the app's registered certificate (the leaf); Key its
	// RSA private key.
	Certificate *x509.Certificate
	Key         *rsa.PrivateKey
	// AssertionAlg is AlgPS256 (the default) or AlgRS256.
	AssertionAlg string

	// HTTP sends the token requests. Required.
	HTTP ports.HTTPClient
	// Clock is the time source; the wall clock when nil.
	Clock ports.Clock
	// Skew is how long before expiry a token is replaced; DefaultSkew when
	// zero. A token living under twice the skew is replaced half way.
	Skew time.Duration
}

// Source mints, caches and refreshes one bot's token. It is safe for
// concurrent use.
type Source struct {
	cfg      Config
	endpoint string
	now      func() time.Time

	mu       sync.Mutex
	have     bool
	tok      Token
	minted   time.Time
	inflight *flight
}

type flight struct {
	done chan struct{}
	tok  Token
	err  error
}

// New validates cfg and returns a Source that has minted nothing yet.
func New(cfg Config) (*Source, error) {
	authority, err := parseAuthority(cfg.Authority)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.AppID == "":
		return nil, errors.New("botauth: app ID is required")
	case cfg.HTTP == nil:
		return nil, errors.New("botauth: no HTTP client")
	case cfg.Secret != "" && (cfg.Certificate != nil || cfg.Key != nil):
		return nil, errors.New("botauth: a client secret or a certificate, not both")
	case cfg.Secret == "" && cfg.Certificate == nil && cfg.Key == nil:
		return nil, errors.New("botauth: a client secret or a certificate is required")
	case cfg.Secret == "" && (cfg.Certificate == nil || cfg.Key == nil):
		return nil, errors.New("botauth: a certificate needs its private key")
	}
	if cfg.Secret == "" {
		if pub, ok := cfg.Certificate.PublicKey.(*rsa.PublicKey); !ok || !pub.Equal(&cfg.Key.PublicKey) {
			return nil, errors.New("botauth: the private key does not belong to the certificate")
		}
	}
	switch cfg.AssertionAlg {
	case "":
		cfg.AssertionAlg = AlgPS256
	case AlgPS256, AlgRS256:
	default:
		return nil, errors.New("botauth: assertion algorithm is PS256 or RS256")
	}
	if cfg.Tenant == "" {
		cfg.Tenant = MultiTenant
	}
	if cfg.Scope == "" {
		cfg.Scope = DefaultScope
	}
	if cfg.Skew <= 0 {
		cfg.Skew = DefaultSkew
	}
	now := time.Now
	if cfg.Clock != nil {
		now = cfg.Clock.Now
	}
	return &Source{
		cfg: cfg, now: now,
		endpoint: authority + "/" + url.PathEscape(cfg.Tenant) + "/oauth2/v2.0/token",
	}, nil
}

func parseAuthority(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultAuthority, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("botauth: authority %q: want an origin such as %s", s, DefaultAuthority)
	}
	loop := strings.EqualFold(u.Hostname(), "localhost")
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		loop = true
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loop) {
		return "", errors.New("botauth: the credential is sent to the authority, so it must be https (http only for loopback)")
	}
	return u.Scheme + "://" + u.Host, nil
}

// Token returns a token that is valid and not within the skew of its
// expiry, minting one when needed. fresh says the Bot Connector refused the
// last token: the cache is bypassed unless that token was minted moments
// ago. Concurrent callers share one mint. Errors are *model.Error:
// Unauthorized when the endpoint refuses the credential, Unavailable or
// RateLimited when it cannot answer.
func (s *Source) Token(ctx context.Context, fresh bool) (Token, error) {
	for {
		now := s.now()
		s.mu.Lock()
		if s.have {
			switch {
			case fresh && now.Sub(s.minted) < freshWindow && now.Before(s.tok.Expiry):
				t := s.tok
				s.mu.Unlock()
				return t, nil
			case !fresh && now.Before(s.refreshAt()):
				t := s.tok
				s.mu.Unlock()
				return t, nil
			}
		}
		f := s.inflight
		lead := f == nil
		if lead {
			f = &flight{done: make(chan struct{})}
			s.inflight = f
		}
		s.mu.Unlock()
		if lead {
			// The mint outlives the caller that started it: others share it.
			go s.run(context.WithoutCancel(ctx), f)
		}
		select {
		case <-f.done:
			if f.err != nil {
				return Token{}, f.err
			}
			return f.tok, nil
		case <-ctx.Done():
			return Token{}, ctx.Err()
		}
	}
}

// refreshAt is when the cached token is replaced. Caller holds mu.
func (s *Source) refreshAt() time.Time {
	life := s.tok.Expiry.Sub(s.minted)
	skew := min(s.cfg.Skew, life/2)
	return s.tok.Expiry.Add(-skew)
}

func (s *Source) run(ctx context.Context, f *flight) {
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	tok, err := s.mint(ctx)
	s.mu.Lock()
	if err == nil {
		s.have, s.tok, s.minted = true, tok, s.now()
	}
	s.inflight = nil
	s.mu.Unlock()
	f.tok, f.err = tok, err
	close(f.done)
}

func (s *Source) mint(ctx context.Context) (Token, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"client_id":  {s.cfg.AppID},
		"scope":      {s.cfg.Scope},
	}
	if s.cfg.Secret != "" {
		form.Set("client_secret", s.cfg.Secret)
	} else {
		a, err := s.assertion()
		if err != nil {
			return Token{}, model.NewError(model.Validation, "the client assertion could not be signed")
		}
		form.Set("client_assertion_type", assertionType)
		form.Set("client_assertion", a)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, model.NewError(model.Validation, "invalid token endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	start := s.now()
	res, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return Token{}, model.NewError(model.Transient, "token endpoint unreachable")
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return Token{}, model.NewError(model.Transient, "token endpoint answer lost")
	}
	if res.StatusCode != http.StatusOK {
		return Token{}, statusError(res, raw)
	}
	var ok struct {
		TokenType   string `json:"token_type"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &ok) != nil || ok.AccessToken == "" || ok.ExpiresIn <= 0 ||
		(ok.TokenType != "" && !strings.EqualFold(ok.TokenType, "Bearer")) {
		return Token{}, model.NewError(model.Transient, "token endpoint answer malformed")
	}
	return Token{Value: ok.AccessToken, Expiry: start.Add(time.Duration(ok.ExpiresIn) * time.Second)}, nil
}

var (
	plainCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	aadsts    = regexp.MustCompile(`AADSTS[0-9]{1,8}`)
)

// statusError maps a non-200 answer of the token endpoint, naming only the
// error code and AADSTS code, never the body
// (https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow#error-response).
func statusError(res *http.Response, raw []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	detail := ""
	if plainCode.MatchString(e.Error) {
		detail = ": " + e.Error
		if c := aadsts.FindString(e.Description); c != "" {
			detail += " " + c
		}
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		me := model.NewError(model.RateLimited, "token endpoint rate limit")
		if s := res.Header.Get("Retry-After"); s != "" {
			var n int
			if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 && n <= 600 {
				me.RetryAfter = time.Duration(n) * time.Second
			}
		}
		return me
	case res.StatusCode >= 500:
		return model.NewError(model.Unavailable, "token endpoint unavailable")
	case res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized:
		return &model.Error{Code: model.Unauthorized, SafeMessage: "the token endpoint refused the bot's credential" + detail, Retry: model.NoRetry}
	}
	return &model.Error{Code: model.Unavailable, SafeMessage: fmt.Sprintf("token endpoint answered HTTP %d%s", res.StatusCode, detail), Retry: model.NoRetry}
}

// assertion signs the client assertion for one mint.
func (s *Source) assertion() (string, error) {
	now := s.now()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	claims := jwt.MapClaims{
		"aud": s.endpoint, "iss": s.cfg.AppID, "sub": s.cfg.AppID,
		"jti": hex.EncodeToString(id[:]),
		"nbf": now.Unix(), "iat": now.Unix(), "exp": now.Add(assertTTL).Unix(),
	}
	var (
		method jwt.SigningMethod = jwt.SigningMethodPS256
		hdr                      = map[string]any{}
	)
	sum256 := sha256.Sum256(s.cfg.Certificate.Raw)
	hdr["x5t#S256"] = base64.RawURLEncoding.EncodeToString(sum256[:])
	if s.cfg.AssertionAlg == AlgRS256 {
		method = jwt.SigningMethodRS256
		sum1 := sha1.Sum(s.cfg.Certificate.Raw) // #nosec G401 -- x5t identifier
		hdr["x5t"] = base64.RawURLEncoding.EncodeToString(sum1[:])
	}
	t := jwt.NewWithClaims(method, claims)
	for k, v := range hdr {
		t.Header[k] = v
	}
	var key crypto.Signer = s.cfg.Key
	return t.SignedString(key)
}
