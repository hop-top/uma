package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/model"
)

// maxBodyBytes bounds a webhook body the handler reads, under any lower
// limit the host's mount applies. Meta batches up to 1000 updates.
const maxBodyBytes = 4 << 20

// notification is a webhook event notification.
type notification struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value value  `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type value struct {
	MessagingProduct string `json:"messaging_product"`
	Metadata         struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		PhoneNumberID      string `json:"phone_number_id"`
	} `json:"metadata"`
	Contacts []struct {
		Profile struct {
			Name string `json:"name"`
		} `json:"profile"`
		WaID string `json:"wa_id"`
	} `json:"contacts"`
	Messages []message `json:"messages"`
	Statuses []status  `json:"statuses"`
}

type message struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Text      *struct {
		Body string `json:"body"`
	} `json:"text"`
	Context *struct {
		From string `json:"from"`
		ID   string `json:"id"`
	} `json:"context"`
	Reaction *struct {
		MessageID string `json:"message_id"`
		Emoji     string `json:"emoji"`
	} `json:"reaction"`
	Button *struct {
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"button"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *reply `json:"button_reply"`
		ListReply   *reply `json:"list_reply"`
	} `json:"interactive"`
	Image    *media `json:"image"`
	Audio    *media `json:"audio"`
	Video    *media `json:"video"`
	Document *media `json:"document"`
	Sticker  *media `json:"sticker"`
}

type reply struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type media struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	Caption  string `json:"caption,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type status struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
	RecipientID  string `json:"recipient_id"`
	CallbackData string `json:"biz_opaque_callback_data"`
	Conversation *struct {
		ID                  string `json:"id"`
		ExpirationTimestamp string `json:"expiration_timestamp"`
		Origin              struct {
			Type string `json:"type"`
		} `json:"origin"`
	} `json:"conversation"`
	Pricing *struct {
		Billable     bool   `json:"billable"`
		PricingModel string `json:"pricing_model"`
		Category     string `json:"category"`
	} `json:"pricing"`
	Errors []struct {
		Code  int    `json:"code"`
		Title string `json:"title"`
	} `json:"errors"`
}

// Status is the StatusSchema payload of a status envelope: delivery
// evidence for the outbound message Envelope.MessageID names.
type Status struct {
	// Status is Meta's value: sent, delivered, read, played or failed.
	Status string `json:"status"`
	// State is the delivery state it evidences: provider_accepted for
	// sent, delivered for delivered, read and played, failed for failed.
	State model.DeliveryState `json:"state"`
	// Read is set for read and played.
	Read bool `json:"read,omitempty"`
	// Recipient is the WhatsApp user the message went to.
	Recipient string `json:"recipient"`
	// Operation and Attempt are the engine operation and attempt the send
	// carried in biz_opaque_callback_data, set only when the echoed value
	// is one the adapter wrote; the envelope then also references
	// model.EchoRef for the attempt.
	Operation model.ID `json:"operation,omitempty"`
	Attempt   model.ID `json:"attempt,omitempty"`
	// Failure summarizes the first error of a failed status.
	Failure *model.DeliveryFailure `json:"failure,omitempty"`
	// ErrorCodes are the Cloud API error codes of a failed status.
	ErrorCodes []int `json:"error_codes,omitempty"`
	// ConversationID, ConversationExpiresAt and Category describe the
	// pricing conversation Meta opened, when reported.
	ConversationID        string    `json:"conversation_id,omitempty"`
	ConversationExpiresAt time.Time `json:"conversation_expires_at,omitzero"`
	Category              string    `json:"category,omitempty"`
	Billable              bool      `json:"billable,omitempty"`
}

var deliveryStates = map[string]model.DeliveryState{
	"sent": model.ProviderAccepted, "delivered": model.Delivered, "read": model.Delivered,
	"played": model.Delivered, "failed": model.Failed,
}

// Handler returns the Meta-facing webhook handler. It answers:
//
//   - GET: the subscription verification. 200 with hub.challenge as the
//     body when hub.mode is subscribe and hub.verify_token matches the
//     channel's verify token; 403 otherwise; 503 when the token cannot be
//     resolved.
//   - POST: an event notification. The raw body is read (413 over the
//     limit, 400 unreadable), the X-Hub-Signature-256 checked in constant
//     time against the app secret (401 missing or wrong, 503 when the
//     secret cannot be resolved) before it is parsed (400 malformed).
//     Only changes whose phone_number_id is the channel's reach the sink.
//     200 once every one was accepted, new or duplicate; 503 when any
//     failure is retryable, so Meta redelivers the batch; 422 for any
//     other failure.
//   - 405 for other methods, 503 once the instance is closed.
//
// Response bodies carry the status text only.
func (x *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if x.isClosed() || sink == nil {
			statusText(w, http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			x.verify(w, r)
		case http.MethodPost:
			x.notify(w, r, sink)
		default:
			statusText(w, http.StatusMethodNotAllowed)
		}
	})
}

func (x *Instance) verify(w http.ResponseWriter, r *http.Request) {
	token, err := x.secret(r.Context(), PurposeVerifyToken)
	if err != nil {
		statusText(w, http.StatusServiceUnavailable)
		return
	}
	challenge, ok := meta.VerifyChallenge(r.URL.Query(), token)
	if !ok {
		statusText(w, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, challenge) // #nosec G705 -- digits only (meta.VerifyChallenge), served as text/plain with nosniff
}

func (x *Instance) notify(w http.ResponseWriter, r *http.Request, sink adapter.Sink) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		statusText(w, http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		statusText(w, http.StatusBadRequest)
		return
	}
	secret, err := x.secret(r.Context(), PurposeAppSecret)
	if err != nil {
		statusText(w, http.StatusServiceUnavailable)
		return
	}
	if !meta.ValidSignature(r.Header.Get(meta.SignatureHeader), body, secret) {
		statusText(w, http.StatusUnauthorized)
		return
	}
	var n notification
	if err := json.Unmarshal(body, &n); err != nil {
		statusText(w, http.StatusBadRequest)
		return
	}
	statusText(w, x.deliver(r.Context(), sink, n))
}

// deliver hands every own change of n to sink and returns the answer.
func (x *Instance) deliver(ctx context.Context, sink adapter.Sink, n notification) int {
	if n.Object != "whatsapp_business_account" {
		return http.StatusOK
	}
	answer := http.StatusOK
	fail := func(err error) {
		if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			answer = http.StatusServiceUnavailable
		} else if answer == http.StatusOK {
			answer = http.StatusUnprocessableEntity
		}
	}
	for _, e := range n.Entry {
		for _, c := range e.Changes {
			v := c.Value
			if c.Field != "messages" || v.Metadata.PhoneNumberID != x.cfg.PhoneNumberID {
				continue
			}
			for _, m := range v.Messages {
				in, ok := x.messageIngress(v, m)
				if !ok {
					continue
				}
				if err := x.recordInbound(ctx, m.From, in.Envelope.OccurredAt); err != nil {
					fail(model.NewError(model.Unavailable, "window state unavailable"))
					continue
				}
				if err := x.attachMedia(ctx, sink, m, &in); err != nil {
					fail(err)
					continue
				}
				if _, err := sink.Accept(ctx, in); err != nil {
					fail(err)
				}
			}
			for _, s := range v.Statuses {
				in, ok := x.statusIngress(s)
				if !ok {
					continue
				}
				if _, err := sink.Accept(ctx, in); err != nil {
					fail(err)
				}
			}
		}
	}
	return answer
}

func (x *Instance) user(id string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: RouteKind, ID: id}
}

func (x *Instance) occurred(ts string) time.Time {
	if s, err := strconv.ParseInt(ts, 10, 64); err == nil && s > 0 {
		return time.Unix(s, 0).UTC()
	}
	return x.now().UTC()
}

func ext(name string, version uint32, v any) model.Extension {
	data, _ := json.Marshal(v)
	return model.Extension{Name: name, Version: version, Data: data}
}

// messageIngress maps an inbound message. A reaction removal carries no
// emoji (WhatsApp does not say which was removed): it is a reaction.removed
// without content, resolved by the engine against the sender's one
// reaction on the message.
func (x *Instance) messageIngress(v value, m message) (model.Ingress, bool) {
	if m.ID == "" || m.From == "" {
		return model.Ingress{}, false
	}
	inbound := map[string]any{"type": m.Type}
	for _, c := range v.Contacts {
		if c.WaID == m.From || len(v.Contacts) == 1 {
			inbound["wa_id"], inbound["profile_name"] = c.WaID, c.Profile.Name
		}
	}
	if m.Context != nil {
		inbound["context"] = map[string]string{"from": m.Context.From, "id": m.Context.ID}
	}
	env := model.Envelope{
		EventID: m.ID, MessageID: m.ID, Destination: x.cfg.PhoneNumberID, Kind: model.EnvelopeMessage,
		Sender: x.user(m.From), Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: x.occurred(m.Timestamp),
		Payload: ext(InboundSchema, InboundVersion, inbound),
	}
	if m.Type == "reaction" && m.Reaction != nil {
		if m.Reaction.MessageID == "" {
			return model.Ingress{}, false
		}
		env.Kind, env.MessageID = model.EnvelopeReactionAdded, m.Reaction.MessageID
		if m.Reaction.Emoji == "" {
			env.Kind = model.EnvelopeReactionRemoved
			return model.Ingress{Envelope: env}, true
		}
		env.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: m.Reaction.Emoji}}}
		return model.Ingress{Envelope: env}, true
	}
	env.Content = content(m)
	if m.Context != nil {
		// A context ID names the message replied to; a forward carries none.
		env.InReplyTo = m.Context.ID
	}
	return model.Ingress{Envelope: env}, true
}

// content maps a message body to parts: text as text, media as a caption
// and a MediaSchema part, anything else as a MessageSchema part with its
// visible text.
func content(m message) model.Content {
	var parts []model.Part
	textPart := func(s string) {
		if s != "" {
			parts = append(parts, model.Part{Kind: model.PartText, Text: s})
		}
	}
	structured := func(e model.Extension) {
		parts = append(parts, model.Part{Kind: model.PartStructured, Structured: &e})
	}
	if m.Type == "text" && m.Text != nil && m.Text.Body != "" {
		textPart(m.Text.Body)
		return model.Content{Parts: parts}
	}
	for typ, md := range map[string]*media{"image": m.Image, "audio": m.Audio, "video": m.Video, "document": m.Document, "sticker": m.Sticker} {
		if m.Type == typ && md != nil {
			textPart(md.Caption)
			structured(ext(MediaSchema, MediaVersion, map[string]string{
				"type": typ, "id": md.ID, "mime_type": md.MimeType, "sha256": md.SHA256, "filename": md.Filename,
			}))
			return model.Content{Parts: parts}
		}
	}
	other := map[string]string{"type": m.Type}
	switch {
	case m.Button != nil:
		textPart(m.Button.Text)
		other["payload"] = m.Button.Payload
	case m.Interactive != nil && m.Interactive.ButtonReply != nil:
		textPart(m.Interactive.ButtonReply.Title)
		other["reply_id"] = m.Interactive.ButtonReply.ID
	case m.Interactive != nil && m.Interactive.ListReply != nil:
		textPart(m.Interactive.ListReply.Title)
		other["reply_id"] = m.Interactive.ListReply.ID
	}
	structured(ext(MessageSchema, MessageVersion, other))
	return model.Content{Parts: parts}
}

// statusIngress maps an outbound message status to delivery evidence.
func (x *Instance) statusIngress(s status) (model.Ingress, bool) {
	state, ok := deliveryStates[s.Status]
	if !ok || s.ID == "" {
		return model.Ingress{}, false
	}
	p := Status{Status: s.Status, State: state, Read: s.Status == "read" || s.Status == "played", Recipient: s.RecipientID}
	refs := []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, s.ID)}
	if op, at, ok := ParseCallbackData(s.CallbackData); ok {
		p.Operation, p.Attempt = op, at
		refs = append(refs, model.EchoRef(x.channel.Ref, model.OperationRef{Tenant: x.channel.Ref.Tenant, ID: op}, at))
	}
	for _, e := range s.Errors {
		p.ErrorCodes = append(p.ErrorCodes, e.Code)
	}
	if state == model.Failed {
		code := -1
		if len(p.ErrorCodes) > 0 {
			code = p.ErrorCodes[0]
		}
		p.Failure = deliveryFailure(code)
	}
	if c := s.Conversation; c != nil {
		p.ConversationID, p.Category = c.ID, c.Origin.Type
		if ts, err := strconv.ParseInt(c.ExpirationTimestamp, 10, 64); err == nil {
			p.ConversationExpiresAt = time.Unix(ts, 0).UTC()
		}
	}
	if pr := s.Pricing; pr != nil {
		p.Billable = pr.Billable
		if pr.Category != "" {
			p.Category = pr.Category
		}
	}
	return model.Ingress{Envelope: model.Envelope{
		EventID: "status/" + s.Status + "/" + s.ID, MessageID: s.ID, Destination: x.cfg.PhoneNumberID,
		Kind: model.EnvelopeStatus, OccurredAt: x.occurred(s.Timestamp), Audience: model.Audience{Visibility: model.Public},
		References: refs,
		Payload:    ext(StatusSchema, StatusVersion, p),
		Delivery:   &model.DeliveryReport{State: state, Failure: p.Failure},
	}}, true
}

func statusText(w http.ResponseWriter, code int) {
	if code == http.StatusOK {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, http.StatusText(code))
		return
	}
	http.Error(w, http.StatusText(code), code)
}
