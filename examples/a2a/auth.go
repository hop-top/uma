package main

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
)

// bearerAuth is the host's authentication of A2A requests: a static map
// from bearer token to agent ID. The adapter holds no credentials and
// verifies nothing; whatever this function returns is the agent a request
// came from, and uma's authorizer still decides every access afterwards.
// A real host verifies a signed token or a client certificate here, and
// declares the same scheme in the channel's security_schemes.
func bearerAuth(tokens map[string]string) a2a.Authenticator {
	return func(r *http.Request, _ model.ChannelRef) (a2a.Principal, error) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return a2a.Principal{}, model.NewError(model.Unauthorized, "bearer token required")
		}
		// Compare with every known token, so the time taken does not tell
		// which one matched.
		agent, found := "", false
		for known, id := range tokens {
			if subtle.ConstantTimeCompare([]byte(known), []byte(token)) == 1 {
				agent, found = id, true
			}
		}
		if !found {
			return a2a.Principal{}, model.NewError(model.Unauthorized, "unknown token")
		}
		return a2a.Principal{ID: agent}, nil
	}
}
