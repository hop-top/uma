package twilio

import (
	"errors"
	"net/http"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Twilio error codes with a meaning beyond their HTTP status.
const (
	// CodePermissionDenied: credentials wrong, expired or scoped elsewhere.
	CodePermissionDenied = 20003
	// CodeTooManyRequests: REST concurrency limit; nothing was processed.
	CodeTooManyRequests = 20429
)

// DeliveryState maps a Twilio message status to a delivery state. ok is
// false for a status Twilio does not document for outbound messages.
//
//   - queued, accepted, scheduled, sending, sent: the provider holds the
//     message (provider_accepted);
//   - delivered, read: delivered;
//   - failed, undelivered, canceled: failed.
func DeliveryState(status string) (model.DeliveryState, bool) {
	switch status {
	case "queued", "accepted", "scheduled", "sending", "sent":
		return model.ProviderAccepted, true
	case "delivered", "read":
		return model.Delivered, true
	case "failed", "undelivered", "canceled":
		return model.Failed, true
	}
	return "", false
}

// Report is the delivery report of a status callback with status and
// Twilio error code (zero when absent), for Envelope.Delivery; nil for a
// status Twilio does not document for outbound messages.
func Report(status string, errorCode int) *model.DeliveryReport {
	state, ok := DeliveryState(status)
	if !ok {
		return nil
	}
	r := &model.DeliveryReport{State: state}
	if state == model.Failed {
		msg := "Twilio reported the message " + status
		if errorCode > 0 {
			msg += " (error " + strconv.Itoa(errorCode) + ")"
		}
		r.Failure = &model.DeliveryFailure{Code: model.Permanent, Retry: model.NoRetry, SafeMessage: msg}
	}
	return r
}

// SendOutcome maps the answer to a create call on channel ch to an adapter
// outcome:
//
//   - a message resource: its delivery state, with the message SID as the
//     provider reference (an undocumented status still has a SID, so it
//     counts as provider_accepted); failed carries Twilio's error code;
//   - 429 or error 20429: rate_limited with the stated Retry-After, the
//     only answer after send that is safe to retry (Twilio documents 429
//     requests as not processed);
//   - 5xx: outcome_unknown, since Twilio had the request;
//   - 401, 403 or error 20003: unauthorized, never retried;
//   - 400: rejected, permanent, with Twilio's error code;
//   - any other 4xx: failed, permanent;
//   - a request that provably never reached Twilio (connection or TLS
//     failure before anything was written): transient;
//   - a request that may have been sent with no readable answer (timeout,
//     reset, unreadable body): outcome_unknown, reconcile first.
func SendOutcome(ch model.ChannelRef, m Message, err error) (adapter.Outcome, error) {
	if err != nil {
		return failure(err)
	}
	if m.SID == "" {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Twilio answer carries no message SID")
	}
	refs := []model.ExternalRef{model.ProviderMessageRef(ch, m.SID)}
	state, ok := DeliveryState(m.Status)
	if !ok {
		state = model.ProviderAccepted
	}
	out := adapter.Outcome{State: state, References: refs}
	if state == model.Failed {
		out.Failure = &model.Error{Code: model.Permanent, SafeMessage: "Twilio message " + m.Status + codeSuffix(m.ErrorCode), Retry: model.NoRetry}
	}
	return out, nil
}

func failure(err error) (adapter.Outcome, error) {
	var me *model.Error
	if errors.As(err, &me) {
		return adapter.Outcome{}, me
	}
	var re *RequestError
	if errors.As(err, &re) {
		if re.Sent {
			return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Twilio answer lost; the message may exist")
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "Twilio request not sent")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Twilio call failed")
	}
	code := codeSuffix(&ae.Code)
	switch {
	case ae.Status == http.StatusTooManyRequests || ae.Code == CodeTooManyRequests:
		return adapter.Outcome{}, &model.Error{
			Code: model.RateLimited, SafeMessage: "Twilio rate limit" + code,
			Retry: model.RetrySameOperation, RetryAfter: ae.RetryAfter,
		}
	case ae.Status >= 500:
		// Create has no idempotency key: once Twilio has the request, only a refusal it documents as unprocessed (429) is safe to retry.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Twilio server error after send (HTTP "+strconv.Itoa(ae.Status)+")"+code+"; the message may exist")
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden || ae.Code == CodePermissionDenied:
		return adapter.Outcome{}, &model.Error{Code: model.Unauthorized, SafeMessage: "Twilio refused the credentials" + code, Retry: model.NoRetry}
	case ae.Status == http.StatusBadRequest:
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Permanent, SafeMessage: "Twilio rejected the message" + code, Retry: model.NoRetry,
		}}, nil
	}
	return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "Twilio refused the request (HTTP " + strconv.Itoa(ae.Status) + ")" + code, Retry: model.NoRetry}
}

func codeSuffix(code *int) string {
	if code == nil || *code == 0 {
		return ""
	}
	return " (error " + strconv.Itoa(*code) + ")"
}
