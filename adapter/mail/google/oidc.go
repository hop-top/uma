package google

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Push token checks. Pub/Sub signs the token with Google's OAuth keys
// (RS256); it may be up to an hour old when it arrives, which its exp
// already bounds.
const (
	maxTokenBytes  = 8 << 10
	tokenLeeway    = time.Minute
	defaultKeysTTL = time.Hour
	maxKeysTTL     = 24 * time.Hour
	// minRefetch bounds key refetches caused by an unknown key ID.
	minRefetch = time.Minute
)

// googleIssuers are the issuers Pub/Sub push tokens carry.
var googleIssuers = map[string]bool{"accounts.google.com": true, "https://accounts.google.com": true}

// errNoToken is a push request without a bearer token; errBadToken one
// whose token fails a check. Neither says which check failed.
var (
	errNoToken  = errors.New("google: push request without a bearer token")
	errBadToken = errors.New("google: push token rejected")
)

// pushClaims are the token claims checked.
type pushClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Exp           float64         `json:"exp"`
	Iat           float64         `json:"iat"`
}

// verifyPush checks the Pub/Sub push request's OIDC token: an RS256
// signature by a current Google key (fetched from the certs URL through
// the channel's HTTP client), issuer accounts.google.com, the configured
// audience, the configured service account email with email_verified, and
// exp and iat against the clock. A key fetch failure is model.Unavailable;
// every check failure is errBadToken.
func (i *instance) verifyPush(ctx context.Context, r *http.Request) error {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return errNoToken
	}
	if len(tok) > maxTokenBytes {
		return errBadToken
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errBadToken
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if !segment(parts[0], &hdr) || hdr.Alg != "RS256" || hdr.Kid == "" {
		return errBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errBadToken
	}
	key, err := i.f.keys.key(ctx, i.c.http, i.now(), hdr.Kid)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return errBadToken
	}
	var c pushClaims
	if !segment(parts[1], &c) {
		return errBadToken
	}
	now := i.now()
	exp := time.Unix(int64(c.Exp), 0)
	iat := time.Unix(int64(c.Iat), 0)
	switch {
	case !googleIssuers[c.Iss],
		!audience(c.Aud, i.cfg.PushAudience),
		subtle.ConstantTimeCompare([]byte(strings.ToLower(c.Email)), []byte(strings.ToLower(i.cfg.PushServiceAccount))) != 1,
		!verified(c.EmailVerified),
		c.Exp <= 0 || now.After(exp.Add(tokenLeeway)),
		c.Iat <= 0 || iat.After(now.Add(tokenLeeway)):
		return errBadToken
	}
	return nil
}

func segment(s string, v any) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && json.Unmarshal(b, v) == nil
}

// audience reports whether aud (a string or an array of strings) names
// want.
func audience(aud json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(aud, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(aud, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// verified reports an email_verified claim of true (or "true").
func verified(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	return s == "true" || s == `"true"`
}

// keyCache holds Google's token signing keys, shared by the channels of a
// factory, for as long as the certs response's Cache-Control max-age says.
type keyCache struct {
	url string

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
	fetched time.Time
}

// key returns the signing key kid, fetching the key set when the cached one
// expired or lacks kid (at most once per minRefetch for an unknown kid).
func (k *keyCache) key(ctx context.Context, httpc ports.HTTPClient, now time.Time, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	fresh := now.Before(k.expires)
	if key, ok := k.keys[kid]; ok && fresh {
		return key, nil
	}
	if fresh && now.Sub(k.fetched) < minRefetch {
		return nil, errBadToken
	}
	keys, ttl, err := fetchKeys(ctx, httpc, k.url)
	k.fetched = now
	if err != nil {
		return nil, err
	}
	k.keys, k.expires = keys, now.Add(ttl)
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, errBadToken
}

// jwks is a JSON Web Key Set of RSA keys.
type jwks struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func fetchKeys(ctx context.Context, httpc ports.HTTPClient, u string) (map[string]*rsa.PublicKey, time.Duration, error) {
	unavailable := &model.Error{Code: model.Unavailable, SafeMessage: "google signing keys unavailable", Retry: model.RetrySameOperation}
	c := &client{http: httpc}
	resp, err := c.send(ctx, request{method: "GET", url: u}, "")
	if err != nil || resp.status != http.StatusOK {
		return nil, 0, unavailable
	}
	var set jwks
	if json.Unmarshal(resp.body, &set) != nil {
		return nil, 0, unavailable
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range set.Keys {
		if j.Kty != "RSA" || j.Kid == "" || (j.Use != "" && j.Use != "sig") || (j.Alg != "" && j.Alg != "RS256") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(j.N)
		e, err2 := base64.RawURLEncoding.DecodeString(j.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() < 3 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[j.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, 0, unavailable
	}
	return keys, maxAge(resp.header), nil
}

// maxAge reads Cache-Control max-age, bounded; absent is defaultKeysTTL.
func maxAge(h http.Header) time.Duration {
	for _, d := range strings.Split(h.Get("Cache-Control"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(d)), "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return min(time.Duration(n)*time.Second, maxKeysTTL)
			}
		}
	}
	return defaultKeysTTL
}
