package teams

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The login side of the stand-in Bot Framework (see fakeTeams): the OpenID
// metadata, the key set and the tokens the Bot Connector service signs
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication).

type fakeKey struct {
	priv         *rsa.PrivateKey
	endorsements []string
	published    bool
}

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
	// otherKey signs tokens with a key nobody published.
	otherKey *rsa.PrivateKey
)

func signingKeys() (*rsa.PrivateKey, *rsa.PrivateKey) {
	keyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if otherKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKey, otherKey
}

func (f *fakeTeams) fetches() (meta, keys int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metaFetches, f.keyFetches
}

// addKey publishes (or holds back) another signing key.
func (f *fakeTeams) addKey(kid string, priv *rsa.PrivateKey, published bool, endorsements ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = fakeKey{priv: priv, endorsements: endorsements, published: published}
}

func (f *fakeTeams) serveLogin(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keysDown {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{})
		return
	}
	switch r.URL.String() {
	case fakeMetadataURL:
		f.metaFetches++
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer": BotFrameworkIssuer, "jwks_uri": fakeKeysURL, "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	case fakeKeysURL:
		f.keyFetches++
		var keys []map[string]any
		for kid, k := range f.keys {
			if !k.published {
				continue
			}
			keys = append(keys, map[string]any{
				"kty": "RSA", "use": "sig", "kid": kid,
				"n":            base64.RawURLEncoding.EncodeToString(k.priv.N.Bytes()),
				"e":            base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.priv.E)).Bytes()),
				"endorsements": k.endorsements,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{})
	}
}

// token signs a Bot Connector token for app and serviceURL, valid an hour
// around the fake's clock; mod adjusts the claims.
func (f *fakeTeams) token(app, serviceURL string, mod ...func(jwt.MapClaims, *string)) string {
	now := f.now()
	claims := jwt.MapClaims{
		"iss": BotFrameworkIssuer, "aud": app, "serviceurl": serviceURL,
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(),
	}
	kid := fakeKid
	for _, m := range mod {
		if m != nil {
			m(claims, &kid)
		}
	}
	f.mu.Lock()
	k, ok := f.keys[kid]
	f.mu.Unlock()
	priv := k.priv
	if !ok {
		_, priv = signingKeys()
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = kid
	s, err := t.SignedString(priv)
	if err != nil {
		panic(err)
	}
	return s
}
