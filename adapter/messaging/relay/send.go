package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Limits on a callback.
const (
	// MaxCallbackBytes bounds the text of one reply.
	MaxCallbackBytes = 64 << 10
	// MaxAnswerBytes bounds how much of the relay's answer is read.
	MaxAnswerBytes = 64 << 10
)

// callbackBody is the JSON one reply is posted as. Its names are fixed: the
// callback is a new endpoint of the operator's gateway, not a field-for-field
// copy of an older sender.
type callbackBody struct {
	// Medium is the channel's medium, sms, whatsapp or imessage.
	Medium string `json:"medium"`
	// To is the recipient's address as the relay reported it: a number, or
	// on iMessage a number or a lowercased email address.
	To   string `json:"to"`
	Body string `json:"body"`
	// InReplyTo is the relay's own id of the message this answers, when the
	// reply names one.
	InReplyTo string `json:"in_reply_to,omitempty"`
	// OperationID is stable across the attempts of one reply and is also the
	// Idempotency-Key header; AttemptID names this call.
	OperationID string `json:"operation_id"`
	AttemptID   string `json:"attempt_id"`
}

// Send posts one reply to the channel's callback and reports what the relay
// answered. Text parts only, joined by newlines; any other part, and any
// non-public audience, is unsupported and fails before a credential is
// resolved or a request made.
//
// The request carries the operation as its Idempotency-Key: a relay that
// remembers keys can recognize a reply it already took. A call that may
// have reached the relay without an answer is outcome_unknown: the engine
// asks the lookup (Reconcile) when the callback names one, and otherwise
// never sends it again (see docs/adapters).
//
// Outcomes by the relay's answer:
//
//   - 2xx: provider_accepted, with the id the answer names (callback
//     message_id keys) or, when it names none, the attempt's;
//   - 3xx: rejected. Redirects are never followed;
//   - 429: rate_limited with Retry-After; 401, 403: unauthorized, the
//     callback credential was refused;
//   - 408, 409 and 5xx: outcome_unknown, since the relay may have applied it;
//   - any other 4xx: rejected, final.
//
// A request that never left the host (dial, DNS or TLS failure) is
// transient; one sent without an answer, a timeout included, is
// outcome_unknown. A caller that gives up gets its own context error.
func (i *Instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	if i.isClosed() {
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	}
	if i.cfg.Callback == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Disabled, SafeMessage: "channel has no callback", Retry: model.NoRetry}
	}
	r := req.Route
	if r.Channel != i.channel.Ref || r.Namespace != i.namespace || r.Kind != RouteKind || !validAddress(r.ID) {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a phone route of this channel", Retry: model.NoRetry}
	}
	if req.Operation.ID == "" || req.Attempt == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "operation and attempt required", Retry: model.NoRetry}
	}
	if v := req.Audience.Visibility; v != "" && v != model.Public {
		return adapter.Outcome{}, &model.Error{Code: model.Unsupported, SafeMessage: "SMS and WhatsApp have no non-public audience", Retry: model.NoRetry}
	}
	text, err := textOf(req.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if i.http == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client", Retry: model.NoRetry}
	}
	secret, err := i.callbackSecret(ctx)
	if err != nil {
		return adapter.Outcome{}, err
	}
	b := callbackBody{
		Medium: i.cfg.Medium, To: r.ID, Body: text,
		OperationID: string(req.Operation.ID), AttemptID: string(req.Attempt),
	}
	if t := req.Target; t != nil && t.Channel == i.channel.Ref && t.Namespace == model.ProviderMessageNamespace && t.Kind == model.RefKindMessage {
		b.InReplyTo = t.ID
	}
	data, err := json.Marshal(b)
	if err != nil {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "reply cannot be encoded", Retry: model.NoRetry}
	}
	return i.post(ctx, req, data, secret)
}

// textOf is the text of c, which must hold text parts only.
func textOf(c model.Content) (string, error) {
	parts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", &model.Error{Code: model.Unsupported, SafeMessage: "relay.messages sends text parts only", Retry: model.NoRetry}
		}
		parts = append(parts, p.Text)
	}
	text := strings.Join(parts, "\n")
	switch {
	case strings.TrimSpace(text) == "":
		return "", &model.Error{Code: model.Validation, SafeMessage: "content required", Retry: model.NoRetry}
	case len(text) > MaxCallbackBytes:
		return "", &model.Error{Code: model.Validation, SafeMessage: "content too large for the relay", Retry: model.NoRetry}
	}
	return text, nil
}

// callbackSecret resolves the callback credential: a secret, or a
// password's secret. Anything else, an empty value or one that cannot be an
// HTTP header value is refused without a request.
func (i *Instance) callbackSecret(ctx context.Context) (string, error) {
	if i.creds == nil {
		return "", &model.Error{Code: model.Unavailable, SafeMessage: "credentials port not configured", Retry: model.NoRetry}
	}
	c, err := i.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: i.channel.Ref, Reference: i.channel.CredentialRef, Purpose: PurposeCallback,
		Accept: []ports.CredentialKind{ports.KindSecret, ports.KindPassword},
	})
	if err != nil {
		return "", err
	}
	var secret []byte
	switch c := c.(type) {
	case ports.Secret:
		secret = c.Bytes
	case ports.Password:
		secret = c.Secret
	default:
		return "", &model.Error{Code: model.Unsupported, SafeMessage: "credential kind not usable for the callback", Retry: model.NoRetry}
	}
	if len(secret) == 0 {
		return "", &model.Error{Code: model.Unauthorized, SafeMessage: "no callback credential", Retry: model.NoRetry}
	}
	for _, b := range secret {
		if b <= ' ' || b > '~' {
			return "", &model.Error{Code: model.Validation, SafeMessage: "callback credential is not a valid header value", Retry: model.NoRetry}
		}
	}
	return string(secret), nil
}

// post sends data to the callback and maps the answer.
func (i *Instance) post(ctx context.Context, send adapter.SendRequest, data []byte, secret string) (adapter.Outcome, error) {
	cctx, cancel := context.WithTimeout(ctx, i.opts.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, i.cfg.Callback.URL, bytes.NewReader(data))
	if err != nil {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "callback request cannot be built", Retry: model.NoRetry}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", string(send.Operation.ID))
	i.authorize(req, secret)
	resp, err := i.http.Do(req)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			// The caller gave up: the request may have been applied.
			return adapter.Outcome{}, ctx.Err()
		case notSent(err):
			return adapter.Outcome{}, model.NewError(model.Transient, "relay unreachable; nothing sent")
		}
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "relay answer lost")
	}
	defer func() { _ = resp.Body.Close() }()
	if answeredElsewhere(req, resp) {
		// The host's client followed a redirect; what answered is not the
		// callback, so it settles nothing.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "relay answer came from another URL")
	}
	return i.outcome(send, resp)
}

// authorize asks for JSON and sets the callback credential the way the
// callback configures it: one header, never the URL.
func (i *Instance) authorize(req *http.Request, secret string) {
	req.Header.Set("Accept", "application/json")
	if i.cfg.Callback.Auth == AuthHeader {
		req.Header.Set(i.cfg.Callback.Header, secret)
	} else {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
}

// answeredElsewhere reports whether the host's client followed a redirect:
// what answered is not the URL asked, so its answer settles nothing.
func answeredElsewhere(req *http.Request, resp *http.Response) bool {
	return resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != req.URL.String()
}

// outcome maps the relay's answer.
func (i *Instance) outcome(send adapter.SendRequest, resp *http.Response) (adapter.Outcome, error) {
	code := resp.StatusCode
	status := strconv.Itoa(code)
	switch {
	case code >= 200 && code < 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxAnswerBytes))
		id := i.answerID(body)
		if id == "" {
			id = string(send.Attempt)
		}
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(i.channel.Ref, id)}}, nil
	case code >= 300 && code < 400:
		return rejected("relay answered with a redirect, which is never followed (http " + status + ")"), nil
	case code == http.StatusTooManyRequests:
		return adapter.Outcome{}, &model.Error{
			Code: model.RateLimited, SafeMessage: "relay rate limit (http 429)", Retry: model.RetrySameOperation,
			RetryAfter: retryAfter(resp.Header, i.clock.Now()),
		}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return adapter.Outcome{}, &model.Error{Code: model.Unauthorized, SafeMessage: "relay refused the callback credential (http " + status + ")", Retry: model.NoRetry}
	case code == http.StatusRequestTimeout || code == http.StatusConflict || code >= 500:
		// The relay may have applied it; only a lookup can tell.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "relay answer lost (http "+status+")")
	}
	return rejected("relay rejected the message (http " + status + ")"), nil
}

func rejected(msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: model.Permanent, SafeMessage: msg, Retry: model.NoRetry}}
}

// answerID reads the relay's id for the message from a JSON answer: the
// first configured key the object holds, if its value is a usable string.
// Empty when the answer names none.
func (i *Instance) answerID(body []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	names := i.cfg.Callback.MessageID
	if len(names) == 0 {
		names = defaults.MessageID
	}
	id, found, ok := optionalString(m, names)
	if !found || !ok || !validMessageID(id) {
		return ""
	}
	return id
}

// retryAfter is the wait a Retry-After header states, in seconds or as an
// HTTP date; zero when it states none.
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

// notSent reports a transport error raised before any request byte was
// written: a refused or unreachable dial, a failed DNS lookup or a failed
// TLS handshake.
func notSent(err error) bool {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &dns) || errors.As(err, &cert) || errors.As(err, &record) || errors.As(err, &alert) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

var _ adapter.Sender = (*Instance)(nil)
