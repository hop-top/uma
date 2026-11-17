package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Lookup answer statuses: the JSON answer's "status".
const (
	// LookupSent: the relay took a callback with the operation's
	// Idempotency-Key and holds it as sent.
	LookupSent = "sent"
	// LookupNotFound: the relay holds no callback with the key and will act
	// on none. uma may then send the reply again.
	LookupNotFound = "not_found"
)

// LookupQuery is the query parameter of a lookup naming the operation: the
// Idempotency-Key value and the operation_id of the callback body.
const LookupQuery = "operation_id"

// lookupStatus names the JSON key of the status in a lookup answer.
var lookupStatus = []string{"status"}

// Reconcile asks the relay's lookup what became of a reply whose answer was
// lost: GET callback.lookup_url?operation_id=<operation ID>, with the
// callback credential sent as for a callback, bounded like one, redirects
// never followed. A lookup changes nothing at the relay, so asking again is
// always safe.
//
// Outcomes by the relay's answer, read up to MaxAnswerBytes as a JSON
// object:
//
//   - 2xx {"status":"sent"}: provider_accepted, with the id the answer
//     names under the callback's message_id keys or, when it names none,
//     the attempt's, as Send reports it;
//   - 2xx or 404 {"status":"not_found"}: failed with not_found. The engine
//     takes it as proof only past the descriptor's ReconcileSettle, and
//     then sends the reply again;
//   - any other 2xx or 404 answer (no JSON, no or another status, over the
//     read bound): state unknown, inconclusive;
//   - 429: rate_limited with Retry-After; 408, 5xx and a transport failure
//     or timeout: transient. The engine asks again later;
//   - 401, 403: unauthorized; 3xx, an answer from another URL and any other
//     4xx: permanent. Each says nothing about the reply, which stays
//     outcome_unknown and is never sent again.
//
// Without a lookup URL it is model.Disabled before any credential is
// resolved or request made. Errors carry the status only, never the answer,
// URL or credential.
func (i *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if i.isClosed() {
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	}
	if i.cfg.Callback == nil || i.cfg.Callback.LookupURL == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Disabled, SafeMessage: "channel has no lookup", Retry: model.NoRetry}
	}
	switch req.Capability {
	case model.CapabilitySend, model.CapabilityStartConversation:
	default:
		return adapter.Outcome{}, &model.Error{Code: model.Unsupported, SafeMessage: "relay.messages reconciles sends only", Retry: model.NoRetry}
	}
	r := req.Route
	if r.Channel != i.channel.Ref || r.Namespace != i.namespace || r.Kind != RouteKind || !validAddress(r.ID) {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a phone route of this channel", Retry: model.NoRetry}
	}
	if req.Operation.ID == "" || req.Attempt == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "operation and attempt required", Retry: model.NoRetry}
	}
	if i.http == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client", Retry: model.NoRetry}
	}
	secret, err := i.callbackSecret(ctx)
	if err != nil {
		return adapter.Outcome{}, err
	}
	return i.lookup(ctx, req, secret)
}

// lookup asks the lookup URL about rec's operation and maps the answer.
func (i *Instance) lookup(ctx context.Context, rec adapter.ReconcileRequest, secret string) (adapter.Outcome, error) {
	u, err := url.Parse(i.cfg.Callback.LookupURL)
	if err != nil {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "lookup request cannot be built", Retry: model.NoRetry}
	}
	u.RawQuery = url.Values{LookupQuery: {string(rec.Operation.ID)}}.Encode()
	cctx, cancel := context.WithTimeout(ctx, i.opts.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "lookup request cannot be built", Retry: model.NoRetry}
	}
	i.authorize(req, secret)
	resp, err := i.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return adapter.Outcome{}, ctx.Err()
		}
		// Sent or not, a lookup changed nothing: ask again later.
		return adapter.Outcome{}, model.NewError(model.Transient, "relay lookup unanswered")
	}
	defer func() { _ = resp.Body.Close() }()
	if answeredElsewhere(req, resp) {
		return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "relay lookup answer came from another URL", Retry: model.NoRetry}
	}
	return i.lookupOutcome(rec, resp)
}

// lookupOutcome maps the lookup's answer.
func (i *Instance) lookupOutcome(rec adapter.ReconcileRequest, resp *http.Response) (adapter.Outcome, error) {
	code := resp.StatusCode
	status := strconv.Itoa(code)
	switch {
	case code >= 200 && code < 300:
		return i.lookupAnswer(rec, resp.Body, true), nil
	case code == http.StatusNotFound:
		return i.lookupAnswer(rec, resp.Body, false), nil
	case code >= 300 && code < 400:
		return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "relay lookup answered with a redirect, which is never followed (http " + status + ")", Retry: model.NoRetry}
	case code == http.StatusTooManyRequests:
		return adapter.Outcome{}, &model.Error{
			Code: model.RateLimited, SafeMessage: "relay lookup rate limit (http 429)", Retry: model.RetrySameOperation,
			RetryAfter: retryAfter(resp.Header, i.clock.Now()),
		}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return adapter.Outcome{}, &model.Error{Code: model.Unauthorized, SafeMessage: "relay refused the lookup credential (http " + status + ")", Retry: model.NoRetry}
	case code == http.StatusRequestTimeout || code >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "relay lookup failed (http "+status+")")
	}
	return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "relay refused the lookup (http " + status + ")", Retry: model.NoRetry}
}

// lookupAnswer reads a lookup answer's JSON. sentAllowed is false for a
// 404, which can say not_found and nothing else.
func (i *Instance) lookupAnswer(rec adapter.ReconcileRequest, r io.Reader, sentAllowed bool) adapter.Outcome {
	body, err := io.ReadAll(io.LimitReader(r, MaxAnswerBytes+1))
	var m map[string]json.RawMessage
	if err != nil || len(body) > MaxAnswerBytes || json.Unmarshal(body, &m) != nil {
		return inconclusive()
	}
	st, found, ok := optionalString(m, lookupStatus)
	switch {
	case !found || !ok:
		return inconclusive()
	case st == LookupNotFound:
		return adapter.Outcome{State: model.Failed, Failure: &model.Error{
			Code: model.NotFound, SafeMessage: "relay holds no callback for the operation", Retry: model.NoRetry,
		}}
	case st == LookupSent && sentAllowed:
		id := i.answerID(body)
		if id == "" {
			id = string(rec.Attempt)
		}
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(i.channel.Ref, id)}}
	}
	return inconclusive()
}

// inconclusive is a lookup answer that proves nothing: the operation stays
// outcome_unknown and is reconciled again.
func inconclusive() adapter.Outcome {
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "relay lookup answer proves nothing")}
}

var _ adapter.Reconciler = (*Instance)(nil)
