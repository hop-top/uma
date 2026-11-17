package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Template is the data of a TemplateSchema part: an approved Content
// Template and its placeholder values.
type Template struct {
	// ContentSID is the Content Template SID (HX...). Required.
	ContentSID string `json:"content_sid"`
	// Variables maps placeholder keys ("1", "2", ...) to their values;
	// Twilio uses the template's defaults for keys left out.
	Variables map[string]string `json:"content_variables,omitempty"`
}

// Part returns t as a structured content part.
func (t Template) Part() model.Part {
	data, _ := json.Marshal(t) // strings only
	return model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: TemplateSchema, Version: TemplateVersion, Data: data}}
}

// Send creates one WhatsApp message to the request's route. Content is
// either text parts, joined by newlines into a free-form Body, or exactly
// one TemplateSchema part, sent as ContentSid and ContentVariables with no
// Body. Anything else, and non-public audiences, fail before any call. A
// free-form message the instance knows to be outside the customer service
// window is rejected before any call; Twilio's own refusal (error 63016)
// is rejected too. The credential is resolved for PurposeREST on every
// call. See twilio.SendOutcome for the outcome of other answers.
func (i *Instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	if i.isClosed() {
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	}
	r := req.Route
	if r.Channel != i.channel.Ref || r.Namespace != Namespace || r.Kind != RouteKind || r.ID == "" || strings.Contains(r.ID, ":") {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a twilio.whatsapp user of this channel", Retry: model.NoRetry}
	}
	if v := req.Audience.Visibility; v != "" && v != model.Public {
		return adapter.Outcome{}, &model.Error{Code: model.Unsupported, SafeMessage: "WhatsApp has no non-public audience", Retry: model.NoRetry}
	}
	m, freeForm, err := render(req.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if freeForm {
		open, known, err := i.windowOpen(ctx, r.ID)
		if err != nil {
			return adapter.Outcome{}, model.NewError(model.Unavailable, "customer service window state unavailable")
		}
		if known && !open {
			return adapter.Outcome{State: model.Rejected, Failure: windowClosed()}, nil
		}
	}
	if i.rest == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for Twilio", Retry: model.NoRetry}
	}
	cred, err := i.resolve(ctx, PurposeREST, ports.KindSecret, ports.KindPassword)
	if err != nil {
		return adapter.Outcome{}, err
	}
	m.To = AddressPrefix + r.ID
	if i.cfg.From != "" {
		m.From = AddressPrefix + i.cfg.From
	}
	m.MessagingServiceSID = i.cfg.MessagingServiceSID
	m.StatusCallback = i.statusCallback(req.Effect)
	sent, err := i.rest.CreateMessage(ctx, i.cfg.AccountSID, cred, m)
	if ae := (*twilio.APIError)(nil); errors.As(err, &ae) && ae.Code == CodeOutsideWindow {
		return adapter.Outcome{State: model.Rejected, Failure: windowClosed()}, nil
	}
	return twilio.SendOutcome(i.channel.Ref, sent, err)
}

// Reconcile looks for the message an attempt whose answer was lost
// created: an outbound message to the request's route from the channel's
// sender with exactly the attempt's free-form body, created at or after
// the attempt was recorded. A template message's body is rendered by
// Twilio and cannot be matched, and without a record time the listing
// cannot exclude older messages: both answers are inconclusive. See
// twilio.ReconcileOutcome.
func (i *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if i.isClosed() {
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	}
	r := req.Route
	if r.Channel != i.channel.Ref || r.Namespace != Namespace || r.Kind != RouteKind || r.ID == "" || strings.Contains(r.ID, ":") {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a twilio.whatsapp user of this channel", Retry: model.NoRetry}
	}
	m, freeForm, err := render(req.Content)
	if err != nil || !freeForm || req.AttemptedAt.IsZero() {
		return adapter.Outcome{State: model.Unknown}, nil
	}
	if i.rest == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for Twilio", Retry: model.NoRetry}
	}
	cred, err := i.resolve(ctx, PurposeREST, ports.KindSecret, ports.KindPassword)
	if err != nil {
		return adapter.Outcome{}, err
	}
	s := twilio.SentMessage{To: AddressPrefix + r.ID, MessagingServiceSID: i.cfg.MessagingServiceSID, Body: m.Body, Since: req.AttemptedAt}
	if i.cfg.From != "" {
		s.From = AddressPrefix + i.cfg.From
	}
	found, ok, err := i.rest.FindSent(ctx, i.cfg.AccountSID, cred, s)
	return twilio.ReconcileOutcome(i.channel.Ref, found, ok, err)
}

// render builds the message for c and reports whether it is free-form.
func render(c model.Content) (twilio.CreateMessage, bool, error) {
	unsupported := func(msg string) (twilio.CreateMessage, bool, error) {
		return twilio.CreateMessage{}, false, &model.Error{Code: model.Unsupported, SafeMessage: msg, Retry: model.NoRetry}
	}
	if len(c.Parts) == 0 {
		return twilio.CreateMessage{}, false, &model.Error{Code: model.Validation, SafeMessage: "content required", Retry: model.NoRetry}
	}
	if p := c.Parts[0]; p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == TemplateSchema {
		if len(c.Parts) != 1 {
			return unsupported("a template message is one " + TemplateSchema + " part")
		}
		return template(*p.Structured)
	}
	texts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return unsupported("twilio.whatsapp sends text parts or one " + TemplateSchema + " part")
		}
		texts = append(texts, p.Text)
	}
	body := strings.Join(texts, "\n")
	if body == "" {
		return twilio.CreateMessage{}, false, &model.Error{Code: model.Validation, SafeMessage: "content required", Retry: model.NoRetry}
	}
	return twilio.CreateMessage{Body: body}, true, nil
}

func template(e model.Extension) (twilio.CreateMessage, bool, error) {
	invalid := &model.Error{Code: model.Validation, SafeMessage: "template needs a content_sid (HX...) and string content_variables", Retry: model.NoRetry}
	if e.Version != TemplateVersion {
		return twilio.CreateMessage{}, false, &model.Error{Code: model.SchemaMismatch, SafeMessage: "want " + TemplateSchema + " version 1", Retry: model.NoRetry}
	}
	var t Template
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.DisallowUnknownFields()
	if dec.Decode(&t) != nil || !twilio.ValidContentSID(t.ContentSID) {
		return twilio.CreateMessage{}, false, invalid
	}
	m := twilio.CreateMessage{ContentSID: t.ContentSID}
	if len(t.Variables) > 0 {
		vars, err := json.Marshal(t.Variables)
		if err != nil {
			return twilio.CreateMessage{}, false, invalid
		}
		m.ContentVariables = string(vars)
	}
	return m, false, nil
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
