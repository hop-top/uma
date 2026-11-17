package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// MediaSID returns the media SID (ME...) of mediaURL when it names a Media
// resource of message messageSID of accountSID on the client's API origin,
// the only place a media download may send the REST credential.
func (c *Client) MediaSID(accountSID, messageSID, mediaURL string) (string, bool) {
	u, err := url.Parse(mediaURL)
	if err != nil || u.Scheme != c.base.Scheme || u.Host != c.base.Host || u.User != nil || u.RawQuery != "" {
		return "", false
	}
	prefix := c.base.Path + "/2010-04-01/Accounts/" + accountSID + "/Messages/" + messageSID + "/Media/"
	sid, ok := strings.CutPrefix(u.Path, prefix)
	if !ok || !validSID(sid, "ME") || !validSID(accountSID, "AC") {
		return "", false
	}
	return sid, true
}

// maxMediaRedirects bounds the redirects one media download follows.
const maxMediaRedirects = 3

// RedirectError is a media download whose redirect was refused: more hops
// than allowed, a Location that is missing or malformed, a hop that is not
// a public https host, or a 3xx answer that is no redirect. The URLs are
// not kept: a redirect target can carry a signature.
type RedirectError struct{ Reason string }

func (e *RedirectError) Error() string { return "twilio: media redirect refused: " + e.Reason }

// unauthenticatedError is a non-2xx answer of a hop that was sent no
// credential, so it says nothing about the REST credential.
type unauthenticatedError struct{ *APIError }

func (e unauthenticatedError) Unwrap() error { return e.APIError }

// Media downloads media sid of message messageSID with the REST
// credential. Twilio may answer with a redirect to its media store, which
// the download follows itself, at most 3 hops: the credential goes only to
// the API origin, a hop elsewhere must be https on the default port to a
// public host (no loopback, private, link-local or otherwise reserved
// address, no credentials in the URL) and carries no credential, and a
// redirect back to the API origin does not get it back. The caller closes
// the body. A non-2xx answer is an *APIError, a refused redirect a
// *RedirectError, a failed request a *RequestError.
//
// The check is on the URL's host as written. A name that resolves to a
// private address passes it: a host that must stop that supplies an HTTP
// client whose dialer refuses such addresses. An *http.Client has its own
// redirect following switched off for this call; a host client of any
// other type must not follow redirects itself, since then the rules above
// are out of the download's hands.
func (c *Client) Media(ctx context.Context, accountSID string, cred ports.Credential, messageSID, sid string) (io.ReadCloser, string, error) {
	if !validSID(accountSID, "AC") || !validSID(sid, "ME") || (!validSID(messageSID, "MM") && !validSID(messageSID, "SM")) {
		return nil, "", model.NewError(model.Validation, "media SID malformed")
	}
	user, pass, err := basicAuth(accountSID, cred)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	next := *c.base
	next.Path += "/2010-04-01/Accounts/" + accountSID + "/Messages/" + messageSID + "/Media/" + sid
	client, withAuth := noFollow(c.http), true
	for hops := 0; ; hops++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next.String(), nil)
		if err != nil {
			cancel()
			return nil, "", &RequestError{Err: err}
		}
		if withAuth {
			req.SetBasicAuth(user, pass)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return nil, "", &RequestError{Sent: true, Err: scrubURL(err)}
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode <= 299:
			return cancelOnClose{resp.Body, cancel}, resp.Header.Get("Content-Type"), nil
		case isRedirect(resp.StatusCode):
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if hops == maxMediaRedirects {
				cancel()
				return nil, "", &RedirectError{Reason: "too many redirects"}
			}
			target, err := next.Parse(loc)
			if loc == "" || err != nil {
				cancel()
				return nil, "", &RedirectError{Reason: "no usable Location"}
			}
			if withAuth, err = c.hop(target, withAuth); err != nil {
				cancel()
				return nil, "", err
			}
			next = *target
		default:
			data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			_ = resp.Body.Close()
			cancel()
			ae := c.apiError(resp, data)
			switch {
			case resp.StatusCode >= 300 && resp.StatusCode <= 399:
				return nil, "", &RedirectError{Reason: "answer is no redirect"}
			case !withAuth:
				return nil, "", unauthenticatedError{ae}
			}
			return nil, "", ae
		}
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// hop judges a request target reached by redirect and reports whether it
// may carry the credential: only a target on the API origin, and only while
// the download never left it. Any other target must be a public https host
// on the default port.
func (c *Client) hop(u *url.URL, withAuth bool) (bool, error) {
	if u.Scheme == c.base.Scheme && u.Host == c.base.Host && u.User == nil {
		return withAuth, nil
	}
	switch {
	case u.Scheme != "https":
		return false, &RedirectError{Reason: "target is not https"}
	case u.User != nil:
		return false, &RedirectError{Reason: "target carries credentials"}
	case u.Port() != "" && u.Port() != "443":
		return false, &RedirectError{Reason: "target is not on the default port"}
	case !publicHost(u.Hostname()):
		return false, &RedirectError{Reason: "target is not a public host"}
	}
	return false, nil
}

// noFollow returns h with its own redirect following off when it is an
// *http.Client, so the download sees every redirect and applies its own
// rules; any other client is used as it is.
func noFollow(h ports.HTTPClient) ports.HTTPClient {
	if hc, ok := h.(*http.Client); ok {
		cp := *hc
		cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &cp
	}
	return h
}

// reserved lists the address ranges that are not on the public internet
// and that a unicast, non-private, non-loopback test does not exclude.
var reserved = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "2001::/32", "2001:db8::/32", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// publicHost reports whether host, as written in a URL, names a public
// place: an address that is globally routable, or a name with a dot and an
// alphabetic last label (so no single-label, numeric or hexadecimal form a
// resolver might read as an address) outside the local and internal
// suffixes.
func publicHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.Contains(host, "%") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			return false
		}
		for _, p := range reserved {
			if p.Contains(ip) {
				return false
			}
		}
		return true
	}
	dot := strings.LastIndexByte(host, '.')
	if dot < 0 || !strings.ContainsAny(host[dot+1:], "abcdefghijklmnopqrstuvwxyz") {
		return false
	}
	for _, s := range []string{".localhost", ".local", ".internal", ".localdomain", ".home.arpa"} {
		if strings.HasSuffix(host, s) {
			return false
		}
	}
	return true
}

// Why a media of an incoming message was not staged though its download
// was tried (MediaGap.Reason).
const (
	// GapGone: Twilio no longer has the media (404, 410), for instance
	// because it was deleted.
	GapGone = "gone"
	// GapUnauthorized: the REST credential is missing, unusable or refused
	// by Twilio (401, 403). The channel's credential needs fixing; nothing
	// is lost at Twilio, which keeps the media until it is deleted.
	GapUnauthorized = "unauthorized"
	// GapRedirect: Twilio answered with a redirect the download refuses to
	// follow (see Client.Media).
	GapRedirect = "redirect"
	// GapFailed: any other answer that a repeat would not change.
	GapFailed = "failed"
)

// MediaGap is a media of an incoming message that was not staged because
// its download failed for good. Index is the media's position in the
// message, as in MediaUrl{n}.
type MediaGap struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// gapReason classifies a failed media download for MediaGap.
func gapReason(err error) string {
	var (
		re *RedirectError
		un unauthenticatedError
		ae *APIError
	)
	switch {
	case errors.As(err, &re):
		return GapRedirect
	case errors.As(err, &un):
		if un.Status == http.StatusNotFound || un.Status == http.StatusGone {
			return GapGone
		}
	case errors.As(err, &ae):
		switch ae.Status {
		case http.StatusNotFound, http.StatusGone:
			return GapGone
		case http.StatusUnauthorized, http.StatusForbidden:
			return GapUnauthorized
		}
	case model.IsCode(err, model.Unauthorized):
		return GapUnauthorized
	}
	return GapFailed
}

// AnnotateMediaGaps adds gaps to the data of a media summary as its
// "unavailable" list, leaving its other fields as they are.
func AnnotateMediaGaps(data json.RawMessage, gaps []MediaGap) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	list, err := json.Marshal(gaps)
	if err != nil {
		return nil, err
	}
	m["unavailable"] = list
	return json.Marshal(m)
}

// cancelOnClose ends a download's context with its body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
