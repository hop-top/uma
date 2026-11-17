package whatsapp

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/model"
)

// Cloud API error codes the adapter treats specially. Every other code
// the provider answers with is a refusal: rejected, never retried.
const (
	codeWindowClosed = 131047
)

var (
	rateLimitCodes   = codes(4, 80007, 130429, 131056)
	authCodes        = codes(0, 3, 10, 190, 131005)
	serverFaultCodes = codes(2, 131000, 131016, 131057, 133004, 2494100)
)

func codes(cs ...int) map[int]bool {
	m := map[int]bool{}
	for _, c := range cs {
		m[c] = true
	}
	return m
}

func isAuth(code int) bool { return authCodes[code] || (code >= 200 && code <= 299) }

// windowClosed is the failure of a free-form message outside the customer
// service window, whether the adapter or Meta refused it.
func windowClosed() *model.Error {
	return &model.Error{Code: model.Permanent, SafeMessage: "outside the 24-hour customer service window: send a template message", Retry: model.NoRetry}
}

// failure classifies a Graph API error answer. rejected reports a
// provider refusal (outcome rejected); otherwise the error is returned as
// the call's error.
func failure(e *meta.Error) (f *model.Error, rejected bool) {
	code := strconv.Itoa(e.Code)
	switch {
	case !e.Parsed:
		return unparsed(e)
	case rateLimitCodes[e.Code]:
		return &model.Error{Code: model.RateLimited, SafeMessage: "whatsapp rate limit (code " + code + ")", Retry: model.RetrySameOperation, RetryAfter: e.RetryAfter}, false
	case isAuth(e.Code):
		return &model.Error{Code: model.Unauthorized, SafeMessage: "whatsapp refused the access token or permission (code " + code + ")", Retry: model.NoRetry}, false
	case e.Code == codeWindowClosed:
		return windowClosed(), true
	case serverFaultCodes[e.Code], e.Status >= 500:
		// Rule: no idempotency key and no reconciler, so a server fault
		// answered after the request was sent may mean the message went out;
		// it is outcome_unknown, never transient (only an unopened
		// connection or a rate limit is retried).
		return model.NewError(model.OutcomeUnknown, "whatsapp server fault after send (code "+code+")"), false
	}
	return &model.Error{Code: model.Permanent, SafeMessage: "whatsapp rejected the message (code " + code + ")", Retry: model.NoRetry}, true
}

// unparsed classifies an answer without a Graph error object, by status.
func unparsed(e *meta.Error) (*model.Error, bool) {
	status := strconv.Itoa(e.Status)
	switch {
	case e.Status == http.StatusTooManyRequests:
		return &model.Error{Code: model.RateLimited, SafeMessage: "whatsapp rate limit (http 429)", Retry: model.RetrySameOperation, RetryAfter: e.RetryAfter}, false
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return &model.Error{Code: model.Unauthorized, SafeMessage: "whatsapp refused the request (http " + status + ")", Retry: model.NoRetry}, false
	case e.Status >= 500:
		// An intermediary answered; the provider may have applied it.
		return model.NewError(model.OutcomeUnknown, "whatsapp answer lost (http "+status+")"), false
	}
	return &model.Error{Code: model.Permanent, SafeMessage: "whatsapp rejected the request (http " + status + ")", Retry: model.NoRetry}, true
}

// outcomeOf maps a failed Graph API call to the send outcome.
func outcomeOf(err error) (adapter.Outcome, error) {
	var te *meta.TransportError
	var ge *meta.Error
	switch {
	case errors.As(err, &te) && !te.Sent:
		return adapter.Outcome{}, model.NewError(model.Transient, "whatsapp unreachable; nothing sent")
	case errors.As(err, &te):
		// Sent without an answer, including a client timeout.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "whatsapp answer lost")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The request may have been applied: the engine records outcome
		// unknown for a cancelled call.
		return adapter.Outcome{}, err
	case errors.As(err, &ge):
		f, rejected := failure(ge)
		if rejected {
			return adapter.Outcome{State: model.Rejected, Failure: f}, nil
		}
		return adapter.Outcome{}, f
	}
	return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "whatsapp call failed")
}

// deliveryFailure is the safe summary of a failed status's first error.
func deliveryFailure(code int) *model.DeliveryFailure {
	f, _ := failure(&meta.Error{Parsed: true, Code: code, Status: http.StatusOK})
	if f.Code == model.OutcomeUnknown {
		// A failed status is definitive: the message was not delivered.
		f = model.NewError(model.Transient, "whatsapp server fault (code "+strconv.Itoa(code)+")")
	}
	return &model.DeliveryFailure{Code: f.Code, Retry: f.Retry, SafeMessage: f.SafeMessage}
}
