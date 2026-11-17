package imessage

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Backend is the service that sends and receives iMessage for the gateway.
// Each one talks HTTP through the *http.Client it was built with.
type Backend interface {
	// Name is the backend's name in logs and paths: blooio, bluebubbles.
	Name() string
	// Send hands one message to the backend. key is the relay's
	// Idempotency-Key, stable across the attempts of one reply: a backend
	// with its own idempotency passes it on. Result.Detail is for the
	// gateway's log and never holds a credential or message text.
	Send(ctx context.Context, m Outbound) Result
	// VerifyWebhook authenticates a webhook request whose body is body. A
	// backend that signs nothing returns nil; the gateway then requires a
	// secret path token (see Config.WebhookToken).
	VerifyWebhook(h http.Header, body []byte) error
	// Messages returns the inbound messages a verified webhook carries: none
	// for an event that is no inbound one-to-one message, such as a delivery
	// update, a group message or the account's own outbound echo.
	Messages(body []byte) ([]Inbound, error)
	// SignsWebhooks reports whether VerifyWebhook checks a signature.
	SignsWebhooks() bool
}

// Outbound is one reply to send.
type Outbound struct {
	// Key is the relay's Idempotency-Key.
	Key string
	// To is the recipient: a phone number or an Apple ID email address.
	To   string
	Text string
}

// Inbound is one message the backend received, in the relay's terms.
type Inbound struct {
	From      string
	To        string
	Body      string
	MessageID string
	Media     []Media
}

// Media is one attachment the backend serves at a URL.
type Media struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
}

// Outcome is what a backend call proved.
type Outcome int

// Backend call outcomes.
const (
	// Unknown: the call may have sent the message; nothing settles it.
	Unknown Outcome = iota
	// Sent: the backend accepted the message (MessageID, when it named one).
	Sent
	// Rejected: the backend refused it for good; sending again changes
	// nothing.
	Rejected
	// NotSent: the backend provably sent nothing and a later attempt may
	// succeed: no connection, a refused credential, a rate limit, no sender
	// available.
	NotSent
)

// Result is the outcome of one backend call.
type Result struct {
	Outcome   Outcome
	MessageID string
	// RetryAfter is the backend's Retry-After, for NotSent.
	RetryAfter time.Duration
	// RateLimited marks a NotSent the backend answered with 429.
	RateLimited bool
	// Detail says why, for the gateway's log: a status, never a body or a
	// credential.
	Detail string
}

// classify maps a transport error: one raised before any request byte left
// (dial, DNS, TLS) sent nothing; any other may have.
func classify(err error, what string) Result {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var op *net.OpError
	switch {
	case errors.As(err, &dns), errors.As(err, &cert), errors.As(err, &record),
		errors.As(err, &op) && op.Op == "dial":
		return Result{Outcome: NotSent, Detail: what + " unreachable"}
	}
	return Result{Outcome: Unknown, Detail: what + " answer lost"}
}

// retryAfter reads a Retry-After header in seconds or as an HTTP date.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
