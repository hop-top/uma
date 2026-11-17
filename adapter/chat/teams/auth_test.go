package teams

import (
	"crypto/rsa"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"hop.top/uma/adapter"
)

// message is a personal-chat message activity from the test user.
func (r *rig) message(id, text string) map[string]any {
	a := r.activityOf("message", id, testUser, testPersonal, "personal")
	a["text"] = text
	return a
}

func bearer(tok string) func(*http.Request) {
	return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+tok) }
}

// signed signs claims with priv under kid.
func signed(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTokenAccepted(t *testing.T) {
	r := newRig(t)
	if rec := r.post(t, r.message("1001", "hi")); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if n := len(r.sink.envelopes()); n != 1 {
		t.Fatalf("envelopes = %d, want 1", n)
	}
	// The keys came through the host's HTTP client: the stand-in served
	// them, and it refuses every other host.
	if meta, keys := r.f.fetches(); meta != 1 || keys != 1 {
		t.Fatalf("metadata and key fetches = %d, %d; want one each through the port", meta, keys)
	}
}

func TestTokenRefused(t *testing.T) {
	_, other := signingKeys()
	cases := []struct {
		name string
		mod  func(jwt.MapClaims, *string)
		raw  func(r *rig) string
		want int
	}{
		{name: "wrong issuer", mod: func(c jwt.MapClaims, _ *string) { c["iss"] = "https://sts.windows.net/x/" }, want: http.StatusUnauthorized},
		{name: "wrong audience", mod: func(c jwt.MapClaims, _ *string) { c["aud"] = "00000000-0000-4000-8000-0000000000ff" }, want: http.StatusUnauthorized},
		{name: "expired past the skew", mod: func(c jwt.MapClaims, _ *string) { c["exp"] = testNow.Add(-6 * time.Minute).Unix() }, want: http.StatusUnauthorized},
		{name: "not yet valid past the skew", mod: func(c jwt.MapClaims, _ *string) { c["nbf"] = testNow.Add(6 * time.Minute).Unix() }, want: http.StatusUnauthorized},
		{name: "no expiry", mod: func(c jwt.MapClaims, _ *string) { delete(c, "exp") }, want: http.StatusUnauthorized},
		{name: "service URL mismatch", mod: func(c jwt.MapClaims, _ *string) { c["serviceurl"] = "https://smba.teams.test/elsewhere/" }, want: http.StatusUnauthorized},
		{name: "no service URL claim", mod: func(c jwt.MapClaims, _ *string) { delete(c, "serviceurl") }, want: http.StatusUnauthorized},
		{name: "bad signature", raw: func(r *rig) string {
			return signed(t, other, fakeKid, jwt.MapClaims{
				"iss": BotFrameworkIssuer, "aud": testAppID, "serviceurl": r.f.serviceURL(testPrefix), "exp": testNow.Add(time.Hour).Unix(),
			})
		}, want: http.StatusUnauthorized},
		{name: "unsigned", raw: func(r *rig) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
				"iss": BotFrameworkIssuer, "aud": testAppID, "serviceurl": r.f.serviceURL(testPrefix), "exp": testNow.Add(time.Hour).Unix(),
			})
			tok.Header["kid"] = fakeKid
			s, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}, want: http.StatusUnauthorized},
		{name: "another algorithm", raw: func(r *rig) string {
			k, _ := signingKeys()
			tok := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{
				"iss": BotFrameworkIssuer, "aud": testAppID, "serviceurl": r.f.serviceURL(testPrefix), "exp": testNow.Add(time.Hour).Unix(),
			})
			tok.Header["kid"] = fakeKid
			s, _ := tok.SignedString(k)
			return s
		}, want: http.StatusUnauthorized},
		{name: "oversize", mod: func(c jwt.MapClaims, _ *string) { c["pad"] = strings.Repeat("x", maxTokenBytes) }, want: http.StatusUnauthorized},
		{name: "not a token", raw: func(*rig) string { return "not.a.token" }, want: http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			tok := ""
			if c.raw != nil {
				tok = c.raw(r)
			} else {
				tok = r.f.token(testAppID, r.f.serviceURL(testPrefix), c.mod)
			}
			if rec := r.post(t, r.message("1001", "hi"), bearer(tok)); rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
			if n := len(r.sink.envelopes()); n != 0 {
				t.Fatalf("a refused request reached the sink %d times", n)
			}
		})
	}
}

func TestEmptyServiceURLMatchesNothing(t *testing.T) {
	r := newRig(t)
	act := r.message("1001", "hi")
	act["serviceUrl"] = ""
	tok := r.f.token(testAppID, "", func(c jwt.MapClaims, _ *string) { delete(c, "serviceurl") })
	if rec := r.post(t, act, bearer(tok)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: a token without serviceurl authenticates no activity", rec.Code)
	}
}

func TestTokenWithinClockSkewAccepted(t *testing.T) {
	r := newRig(t)
	tok := r.f.token(testAppID, r.f.serviceURL(testPrefix), func(c jwt.MapClaims, _ *string) {
		c["exp"] = testNow.Add(-4 * time.Minute).Unix()
	})
	if rec := r.post(t, r.message("1001", "hi"), bearer(tok)); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 within the 5 minute skew", rec.Code)
	}
}

func TestServiceURLComparedWithoutTrailingSlashAndCase(t *testing.T) {
	r := newRig(t)
	tok := r.f.token(testAppID, "HTTPS://SMBA.TEAMS.TEST/acct", nil)
	if rec := r.post(t, r.message("1001", "hi"), bearer(tok)); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
}

func TestRequestWithoutBearerTokenRefused(t *testing.T) {
	for name, header := range map[string]string{"none": "", "basic": "Basic abc", "empty bearer": "Bearer "} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			rec := r.post(t, r.message("1001", "hi"), func(req *http.Request) { req.Header.Set("Authorization", header) })
			if rec.Code != http.StatusUnauthorized || len(r.sink.envelopes()) != 0 {
				t.Fatalf("status %d with %d envelopes, want 401 and none", rec.Code, len(r.sink.envelopes()))
			}
		})
	}
}

func TestKeyNotEndorsedForTheChannelIsForbidden(t *testing.T) {
	r := newRig(t)
	k, _ := signingKeys()
	r.f.addKey("kid-webchat", k, true, "webchat")
	tok := r.f.token(testAppID, r.f.serviceURL(testPrefix), func(_ jwt.MapClaims, kid *string) { *kid = "kid-webchat" })
	if rec := r.post(t, r.message("1001", "hi"), bearer(tok)); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 for a key not endorsed for msteams", rec.Code)
	}
	if len(r.sink.envelopes()) != 0 {
		t.Fatal("a refused request reached the sink")
	}
}

func TestUnknownKeyIDRefreshesTheKeysOnceBounded(t *testing.T) {
	r := newRig(t)
	if rec := r.post(t, r.message("1001", "hi")); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	_, keys := r.f.fetches()
	// A key published after the cache was filled is found by one refresh,
	// once the refetch bound allows it.
	_, other := signingKeys()
	r.f.addKey("kid-rotated", other, true, "msteams")
	rotated := func(_ jwt.MapClaims, kid *string) { *kid = "kid-rotated" }
	if rec := r.post(t, r.message("1002", "hi"), bearer(r.f.token(testAppID, r.f.serviceURL(testPrefix), rotated))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d inside the refetch bound, want 401", rec.Code)
	}
	if _, n := r.f.fetches(); n != keys {
		t.Fatalf("key fetches = %d inside the refetch bound, want %d", n, keys)
	}
	r.clock.t = r.clock.t.Add(minRefetch)
	if rec := r.post(t, r.message("1003", "hi"), bearer(r.f.token(testAppID, r.f.serviceURL(testPrefix), rotated))); rec.Code != http.StatusOK {
		t.Fatalf("status %d after the bound, want 200", rec.Code)
	}
	if _, n := r.f.fetches(); n != keys+1 {
		t.Fatalf("key fetches = %d, want one refresh", n-keys)
	}
	// An unknown key right after makes no fetch at all.
	unknown := func(_ jwt.MapClaims, kid *string) { *kid = "kid-nobody" }
	for i := range 3 {
		rec := r.post(t, r.message("1004", "hi"), bearer(r.f.token(testAppID, r.f.serviceURL(testPrefix), unknown)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("try %d: status %d, want 401", i, rec.Code)
		}
	}
	if _, n := r.f.fetches(); n != keys+1 {
		t.Fatalf("key fetches = %d after unknown keys, want none more", n-keys)
	}
}

func TestKeysReadAgainAfterADay(t *testing.T) {
	r := newRig(t)
	r.post(t, r.message("1001", "hi"))
	r.clock.t = r.clock.t.Add(keysTTL)
	if rec := r.post(t, r.message("1002", "hi")); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if meta, keys := r.f.fetches(); meta != 2 || keys != 2 {
		t.Fatalf("fetches = %d, %d; want the set read again after %v", meta, keys, keysTTL)
	}
}

func TestKeysUnavailableAnswer503(t *testing.T) {
	r := newRig(t)
	r.f.mu.Lock()
	r.f.keysDown = true
	r.f.mu.Unlock()
	if rec := r.post(t, r.message("1001", "hi")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	nohttp := newRig(t, func(d *adapter.Dependencies) { d.HTTP = nil })
	if rec := nohttp.post(t, nohttp.message("1001", "hi")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without an HTTP client: status %d, want 503 and no fetch of its own", rec.Code)
	}
}
