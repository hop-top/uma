package teams

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Inbound token checks, from "Authenticate requests from the Bot Connector
// service"
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication):
// an RS256 JWT in the Authorization header, signed by a key the OpenID
// metadata names, issued by https://api.botframework.com for the bot's App
// ID, within its validity "with a clock skew of 5 minutes", whose serviceUrl
// claim equals the activity's, and whose signing key is endorsed for the
// activity's channel.
const (
	// BotFrameworkIssuer is the issuer of the tokens the Bot Connector
	// service sends.
	BotFrameworkIssuer = "https://api.botframework.com"
	clockSkew          = 5 * time.Minute
	maxTokenBytes      = 16 << 10
	// keysTTL is how long a fetched key set is trusted before it is read
	// again: "all bot instances should refresh their local cache ... at
	// least once every 24 hours".
	keysTTL = 24 * time.Hour
	// minRefetch bounds key refetches an unknown key ID causes: at most one
	// per minute, so a flood of tokens naming invented keys cannot turn
	// into a flood of fetches.
	minRefetch = time.Minute
	// maxKeyDocBytes bounds what the adapter reads of the metadata and the
	// key set.
	maxKeyDocBytes = 1 << 20
)

// errNoToken is a request without a bearer token; errBadToken one whose
// token fails a check. Neither says which check failed.
var (
	errNoToken  = errors.New("teams: request without a bearer token")
	errBadToken = errors.New("teams: token rejected")
	// errNotEndorsed is a valid signature by a key the Bot Framework does
	// not endorse for the activity's channel.
	errNotEndorsed = errors.New("teams: signing key not endorsed for the channel")
)

// botClaims are the claims checked besides the registered ones.
type botClaims struct {
	jwt.RegisteredClaims
	// ServiceURL is the claim the Bot Connector puts the activity's
	// serviceUrl in.
	ServiceURL string `json:"serviceurl"`
}

// authenticate checks the request's Bot Connector token against the
// activity it carries: serviceURL and channelID are the activity's. A key
// fetch failure is model.Unavailable; every check failure is errBadToken.
func (x *Instance) authenticate(ctx context.Context, header string, serviceURL, channelID string) error {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || raw == "" {
		return errNoToken
	}
	if len(raw) > maxTokenBytes {
		return errBadToken
	}
	var fetchErr error
	notEndorsed := false
	var claims botClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(BotFrameworkIssuer),
		jwt.WithAudience(x.cfg.AppID),
		jwt.WithLeeway(clockSkew),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(x.now),
	)
	_, err := parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, err := x.keys.key(ctx, x.http, x.now(), kid)
		if err != nil {
			fetchErr = err
			return nil, err
		}
		if len(k.endorsements) > 0 && !slices.Contains(k.endorsements, channelID) {
			notEndorsed = true
		}
		return k.pub, nil
	})
	if me, ok := errors.AsType[*model.Error](fetchErr); ok {
		return me
	}
	if err != nil || !sameServiceURL(claims.ServiceURL, serviceURL) {
		return errBadToken
	}
	if notEndorsed {
		return errNotEndorsed
	}
	return nil
}

// sameServiceURL compares the token's serviceurl claim with the activity's
// serviceUrl as Microsoft's Teams SDK does: without a trailing "/" and
// case-insensitively. An empty claim matches nothing.
func sameServiceURL(claim, activity string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(s, "/")) }
	return claim != "" && norm(claim) == norm(activity)
}

// signingKey is one key of the set, with the channels it is endorsed for.
type signingKey struct {
	pub          *rsa.PublicKey
	endorsements []string
}

// keyCache holds the Bot Framework signing keys, shared by the channels of
// a factory, for keysTTL.
type keyCache struct {
	metadataURL string

	mu      sync.Mutex
	keys    map[string]signingKey
	expires time.Time
	fetched time.Time
}

// key returns the signing key kid, reading the metadata and the key set
// when the cached set expired or lacks kid: an unknown kid causes at most
// one read per minRefetch.
func (k *keyCache) key(ctx context.Context, httpc ports.HTTPClient, now time.Time, kid string) (signingKey, error) {
	if kid == "" {
		return signingKey{}, errBadToken
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	fresh := now.Before(k.expires)
	if key, ok := k.keys[kid]; ok && fresh {
		return key, nil
	}
	if fresh && now.Sub(k.fetched) < minRefetch {
		return signingKey{}, errBadToken
	}
	keys, err := fetchKeys(ctx, httpc, k.metadataURL)
	k.fetched = now
	if err != nil {
		return signingKey{}, err
	}
	k.keys, k.expires = keys, now.Add(keysTTL)
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return signingKey{}, errBadToken
}

// jwks is a JSON Web Key Set of RSA keys, with the Bot Framework's
// endorsements: the channels each key may sign for.
type jwks struct {
	Keys []struct {
		Kty          string   `json:"kty"`
		Kid          string   `json:"kid"`
		Use          string   `json:"use"`
		Alg          string   `json:"alg"`
		N            string   `json:"n"`
		E            string   `json:"e"`
		Endorsements []string `json:"endorsements"`
	} `json:"keys"`
}

func keysUnavailable() *model.Error {
	return &model.Error{Code: model.Unavailable, SafeMessage: "Bot Framework signing keys unavailable", Retry: model.RetrySameOperation}
}

// fetchKeys reads the OpenID metadata, then the key set its jwks_uri names,
// through the host's HTTP client.
func fetchKeys(ctx context.Context, httpc ports.HTTPClient, metadataURL string) (map[string]signingKey, error) {
	if httpc == nil {
		return nil, keysUnavailable()
	}
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, httpc, metadataURL, &meta); err != nil || !strings.HasPrefix(meta.JWKSURI, "https://") {
		return nil, keysUnavailable()
	}
	var set jwks
	if err := getJSON(ctx, httpc, meta.JWKSURI, &set); err != nil {
		return nil, keysUnavailable()
	}
	keys := map[string]signingKey{}
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
		keys[j.Kid] = signingKey{pub: pub, endorsements: j.Endorsements}
	}
	if len(keys) == 0 {
		return nil, keysUnavailable()
	}
	return keys, nil
}

func getJSON(ctx context.Context, httpc ports.HTTPClient, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxKeyDocBytes))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return errors.New("teams: key document answered " + res.Status)
	}
	return json.Unmarshal(body, out)
}
