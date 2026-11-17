package relay

import (
	"net/http"

	"hop.top/uma/ports"
)

// doer adapts the host's client to a RoundTripper.
type doer struct{ c ports.HTTPClient }

func (d doer) RoundTrip(r *http.Request) (*http.Response, error) { return d.c.Do(r) }

// noRedirect wraps the host's client so that a 3xx answer is returned as it
// is and never followed: a callback credential goes to the configured URL
// and nowhere else. A host client that follows redirects itself is beyond
// this wrapper; see Send for what the adapter still refuses.
func noRedirect(c ports.HTTPClient) *http.Client {
	return &http.Client{
		Transport:     doer{c},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
