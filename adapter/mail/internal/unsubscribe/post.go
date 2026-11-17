package unsubscribe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

const (
	// PostTimeout bounds the whole one-click exchange.
	PostTimeout = 15 * time.Second
	// maxAnswerBytes bounds what is read of the answer, which is discarded.
	maxAnswerBytes = 64 << 10
	userAgent      = "uma-unsubscribe"
	dialTimeout    = 10 * time.Second
)

// errNotPublic is the failure of a connection the guard refused.
var errNotPublic = errors.New("unsubscribe: target does not resolve to a public address")

// Poster makes the RFC 8058 one-click request. The target is chosen by the
// sender of a message, so Poster never uses a client supplied by the host:
// it builds its own, whose connections are guarded. The zero value is
// production.
type Poster struct {
	// Resolver looks up a host name; the system resolver when nil.
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial opens the connection to an address the guard approved ("ip:port");
	// a net.Dialer with a 10 second timeout when nil. A test stand-in
	// replaces it to reach a local server: the guard still runs first.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs are the roots the target's certificate is verified against; the
	// system roots when nil.
	RootCAs *x509.CertPool
}

// Post sends the one-click POST to target, an https URI Decide approved. It
// sends the form "List-Unsubscribe=One-Click" and nothing else: no
// cookies, no authorization, no referrer, no body from the message.
//
// The connection is guarded at connect time: the host is resolved here, every
// address it resolves to must be public (see PublicAddr; a name with a
// loopback, private, link-local, metadata, shared, unspecified, multicast,
// reserved, IPv4-mapped or NAT64 address among its answers is refused as a
// whole), and only a vetted address is dialed, so a name that changes its
// answer between a check and the dial reaches nothing it was not vetted for.
// No proxy is used, whatever the environment says. A redirect is never
// followed: the answer is final and refused, since RFC 8058 section 3.1 has
// senders not redirect and following one would let the endpoint choose
// another place to be asked. No cookie jar, no connection reuse.
//
// The request is idempotent (unsubscribing again is a no-op to a conforming
// sender), so a lost or retryable answer is transient and the engine may
// repeat the attempt:
//
//   - 2xx: applied (provider_accepted);
//   - 3xx: rejected, validation ("redirect not followed");
//   - a target that resolves to a non-public address: rejected, validation;
//   - 408, 429 and 5xx, or no answer: transient;
//   - any other 4xx: rejected, permanent.
func (p Poster) Post(ctx context.Context, target string) (adapter.Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, PostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(OneClickBody))
	if err != nil {
		return Rejected(model.Validation, "the https URI is not valid"), nil
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	tr := p.transport()
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Jar:           nil,
		Timeout:       PostTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, errNotPublic):
			return Rejected(model.Validation, "the https URI does not resolve to a public host"), nil
		case errors.Is(err, context.Canceled):
			return adapter.Outcome{}, err
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "the unsubscribe endpoint did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
	switch s := resp.StatusCode; {
	case s/100 == 2:
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	case s/100 == 3:
		return Rejected(model.Validation, "the unsubscribe endpoint answered with a redirect, which is not followed"), nil
	case s == http.StatusRequestTimeout || s == http.StatusTooManyRequests || s >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "the unsubscribe endpoint answered with an error that may pass")
	default:
		return Rejected(model.Permanent, "the unsubscribe endpoint refused the request"), nil
	}
}

// transport is the guarded transport: no proxy (the environment's included),
// no connection reuse, bounded handshake and header waits.
func (p Poster) transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           p.guardedDial,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.RootCAs},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: PostTimeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
	}
}

// guardedDial resolves addr's host, refuses it unless every address is
// public, and dials the vetted addresses in order.
func (p Poster) guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		lookup := p.Resolver
		if lookup == nil {
			lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		if ips, err = lookup(ctx, host); err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, errNotPublic
	}
	for _, ip := range ips {
		if !PublicAddr(ip) {
			return nil, errNotPublic
		}
	}
	dial := p.Dial
	if dial == nil {
		d := net.Dialer{Timeout: dialTimeout}
		dial = d.DialContext
	}
	var last error
	for _, ip := range ips {
		c, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// Rejected is a final rejected outcome.
func Rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}}
}
