package whatsapp

import (
	"context"
	"encoding/json"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// sendBody is the Cloud API send message request.
type sendBody struct {
	MessagingProduct string          `json:"messaging_product"`
	RecipientType    string          `json:"recipient_type"`
	To               string          `json:"to"`
	Type             string          `json:"type"`
	Context          *replyContext   `json:"context,omitempty"`
	Text             *textBody       `json:"text,omitempty"`
	Template         json.RawMessage `json:"template,omitempty"`
	Reaction         *reactionBody   `json:"reaction,omitempty"`
	CallbackData     string          `json:"biz_opaque_callback_data,omitempty"`
}

type replyContext struct {
	MessageID string `json:"message_id"`
}

type textBody struct {
	Body       string `json:"body"`
	PreviewURL bool   `json:"preview_url"`
}

// sendAnswer is the Cloud API send message response.
type sendAnswer struct {
	Messages []struct {
		ID     string `json:"id"`
		Status string `json:"message_status"`
	} `json:"messages"`
}

// callbackPrefix marks biz_opaque_callback_data the adapter wrote.
const callbackPrefix = "uma1:"

// MaxCallbackData is the documented maximum length of
// biz_opaque_callback_data.
const MaxCallbackData = 512

// CallbackData is the biz_opaque_callback_data a send carries for an
// effect: its attempt ID and the attempt's operation ID, which Meta echoes
// in status notifications. It is empty when the effect names no attempt
// or the value would exceed MaxCallbackData.
func CallbackData(e adapter.Effect) string {
	if e.Attempt == "" || e.Operation.ID == "" {
		return ""
	}
	s := callbackPrefix + string(e.Operation.ID) + ":" + string(e.Attempt)
	if len(s) > MaxCallbackData {
		return ""
	}
	return s
}

// ParseCallbackData returns the operation and attempt IDs CallbackData
// encoded. Any other value, including one set by another sender on the
// same number, is false: an attempt is never guessed.
func ParseCallbackData(s string) (operation, attempt model.ID, ok bool) {
	rest, ok := strings.CutPrefix(s, callbackPrefix)
	if !ok {
		return "", "", false
	}
	op, at, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", false
	}
	opID, err1 := model.ParseID(op)
	atID, err2 := model.ParseID(at)
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	return opID, atID, true
}

// Send posts one message to the effect's stored route. Content is either
// one text part (a free-form message) or one structured TemplateSchema
// part (a template message); anything else is refused without a call. A
// free-form message the instance knows to be outside the customer service
// window is rejected without a call.
//
// Outcomes: accepted is provider_accepted (queued when Meta holds the
// message) with the message's wamid as reference; refusals are rejected;
// rate limits are rate_limited with the stated wait; refused tokens and
// permissions are unauthorized; only a request that never left the host
// (dial, DNS or TLS handshake failure) is transient; a server fault (5xx
// or a Graph server-error code) or no answer (connection lost, timeout)
// after the request was sent is outcome_unknown, since the message may
// have gone out and is never resent.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.userRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	body, freeForm, err := render(r.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	body.To = route.ID
	body.CallbackData = CallbackData(r.Effect)
	if t := r.Target; t != nil && t.Channel == x.channel.Ref && t.Namespace == model.ProviderMessageNamespace && t.Kind == model.RefKindMessage {
		body.Context = &replyContext{MessageID: t.ID}
	}
	return x.post(ctx, route, body, freeForm)
}

// userRoute checks that route is a WhatsApp user of this channel.
func (x *Instance) userRoute(route model.ExternalRef) (model.ExternalRef, error) {
	if route.Channel != x.channel.Ref || route.Namespace != Namespace || route.Kind != RouteKind || route.ID == "" {
		return model.ExternalRef{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a whatsapp user of this channel", Retry: model.NoRetry}
	}
	return route, nil
}

// post sends body to route's user. A free-form body the instance knows to
// be outside the customer service window is rejected without a call.
func (x *Instance) post(ctx context.Context, route model.ExternalRef, body sendBody, freeForm bool) (adapter.Outcome, error) {
	if freeForm {
		open, known, err := x.windowOpen(ctx, route.ID)
		if err != nil {
			return adapter.Outcome{}, model.NewError(model.Unavailable, "customer service window state unavailable")
		}
		if known && !open {
			return adapter.Outcome{State: model.Rejected, Failure: windowClosed()}, nil
		}
	}
	if x.graph.HTTP == nil {
		return adapter.Outcome{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client", Retry: model.NoRetry}
	}
	token, err := x.secret(ctx, PurposeGraph)
	if err != nil {
		return adapter.Outcome{}, err
	}
	resp, err := x.graph.Post(ctx, x.cfg.PhoneNumberID+"/messages", string(token), body)
	if err != nil {
		return outcomeOf(err)
	}
	var a sendAnswer
	if json.Unmarshal(resp.Body, &a) != nil || len(a.Messages) == 0 || a.Messages[0].ID == "" {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "whatsapp answer carried no message id")
	}
	m := a.Messages[0]
	state := model.ProviderAccepted
	if m.Status == "held_for_quality_assessment" || m.Status == "paused" {
		state = model.Queued
	}
	return adapter.Outcome{State: state, References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, m.ID)}}, nil
}

// render builds the request body for content and reports whether it is a
// free-form (non-template) message.
func render(c model.Content) (sendBody, bool, error) {
	b := sendBody{MessagingProduct: "whatsapp", RecipientType: "individual"}
	unsupported := func(msg string) (sendBody, bool, error) {
		return sendBody{}, false, &model.Error{Code: model.Unsupported, SafeMessage: msg, Retry: model.NoRetry}
	}
	if len(c.Parts) != 1 {
		return unsupported("whatsapp sends one text or one template part per message")
	}
	p := c.Parts[0]
	switch {
	case p.Kind == model.PartText && p.Text != "":
		b.Type, b.Text = "text", &textBody{Body: p.Text}
		return b, true, nil
	case p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == TemplateSchema && p.Structured.Version == TemplateVersion:
		var t struct {
			Name     string `json:"name"`
			Language struct {
				Code string `json:"code"`
			} `json:"language"`
		}
		if json.Unmarshal(p.Structured.Data, &t) != nil || t.Name == "" || t.Language.Code == "" {
			return sendBody{}, false, &model.Error{Code: model.Validation, SafeMessage: "template needs name and language.code", Retry: model.NoRetry}
		}
		b.Type, b.Template = "template", json.RawMessage(p.Structured.Data)
		return b, false, nil
	case p.Kind == model.PartAttachment:
		return unsupported("whatsapp media sends are not supported")
	}
	return unsupported("whatsapp sends text or " + TemplateSchema + " content only")
}
