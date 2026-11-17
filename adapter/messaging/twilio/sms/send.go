package sms

import (
	"context"
	"net/url"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Callback query parameters on the status callback URL. Twilio signs the
// URL it calls, so a verified status callback authenticates them.
const (
	callbackOperation = "operation"
	callbackAttempt   = "attempt"
)

// Send creates one SMS to the request's route. The body is the request's
// text parts joined by newlines; other parts and non-public audiences are
// unsupported and fail before any call. The credential is resolved for
// PurposeREST on every call. See twilio.SendOutcome for the outcome of
// each Twilio answer.
func (i *Instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	if i.isClosed() {
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	}
	r := req.Route
	if r.Channel != i.channel.Ref || r.Namespace != Namespace || r.Kind != RouteKind || r.ID == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a twilio.sms phone route of this channel", Retry: model.NoRetry}
	}
	if v := req.Audience.Visibility; v != "" && v != model.Public {
		return adapter.Outcome{}, &model.Error{Code: model.Unsupported, SafeMessage: "SMS has no non-public audience", Retry: model.NoRetry}
	}
	body, err := text(req.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if i.rest == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for Twilio", Retry: model.NoRetry}
	}
	cred, err := i.resolve(ctx, PurposeREST, ports.KindSecret, ports.KindPassword)
	if err != nil {
		return adapter.Outcome{}, err
	}
	m, err := i.rest.CreateMessage(ctx, i.cfg.AccountSID, cred, twilio.CreateMessage{
		To: r.ID, From: i.cfg.From, MessagingServiceSID: i.cfg.MessagingServiceSID,
		Body: body, StatusCallback: i.statusCallback(req.Effect),
	})
	return twilio.SendOutcome(i.channel.Ref, m, err)
}

// Reconcile looks for the message an attempt whose answer was lost
// created: an outbound message to the request's route from the channel's
// sender with exactly the attempt's body, created at or after the attempt
// was recorded. Without a record time the listing cannot exclude older
// messages, and the answer is inconclusive. See twilio.ReconcileOutcome.
func (i *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if i.isClosed() {
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	}
	r := req.Route
	if r.Channel != i.channel.Ref || r.Namespace != Namespace || r.Kind != RouteKind || r.ID == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a twilio.sms phone route of this channel", Retry: model.NoRetry}
	}
	body, err := text(req.Content)
	if err != nil || req.AttemptedAt.IsZero() {
		return adapter.Outcome{State: model.Unknown}, nil
	}
	if i.rest == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for Twilio", Retry: model.NoRetry}
	}
	cred, err := i.resolve(ctx, PurposeREST, ports.KindSecret, ports.KindPassword)
	if err != nil {
		return adapter.Outcome{}, err
	}
	m, found, err := i.rest.FindSent(ctx, i.cfg.AccountSID, cred, twilio.SentMessage{
		To: r.ID, From: i.cfg.From, MessagingServiceSID: i.cfg.MessagingServiceSID, Body: body, Since: req.AttemptedAt,
	})
	return twilio.ReconcileOutcome(i.channel.Ref, m, found, err)
}

// statusCallback is the webhook URL tagged with the effect's operation and
// attempt, or empty without a webhook URL.
func (i *Instance) statusCallback(e adapter.Effect) string {
	if i.cfg.WebhookURL == "" {
		return ""
	}
	q := url.Values{}
	if e.Operation.ID != "" {
		q.Set(callbackOperation, string(e.Operation.ID))
	}
	if e.Attempt != "" {
		q.Set(callbackAttempt, string(e.Attempt))
	}
	if len(q) == 0 {
		return i.cfg.WebhookURL
	}
	return i.cfg.WebhookURL + "?" + q.Encode()
}

// text returns the text of c, which must hold text parts only.
func text(c model.Content) (string, error) {
	if len(c.Parts) == 0 {
		return "", &model.Error{Code: model.Validation, SafeMessage: "content required", Retry: model.NoRetry}
	}
	parts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", &model.Error{Code: model.Unsupported, SafeMessage: "twilio.sms sends text parts only", Retry: model.NoRetry}
		}
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "\n"), nil
}
