package whatsapp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// emptyTwiML answers an incoming message without replying to it.
const emptyTwiML = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`

// maxMedia bounds the media entries read from one incoming message.
const maxMedia = twilio.MaxMedia

// Handler returns the handler Twilio calls for incoming WhatsApp messages
// and status callbacks, form-encoded POSTs or JSON ones. Before the sink
// sees anything it resolves the auth token (PurposeWebhook) and verifies
// X-Twilio-Signature over the configured WebhookURL plus the query the
// request arrived with and, for a form, every POST parameter. A JSON body is
// signed by the URL alone, which must carry the body's SHA-256 as bodySHA256
// (twilio.VerifyJSON); its top-level scalar fields are read under the form
// parameter names. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable, malformed or incomplete body or one
//     whose From or To is not a WhatsApp address;
//   - 403 for a missing or wrong signature or another account's request;
//   - 503 without a WebhookURL, when the instance is closed, the token
//     cannot be resolved, a download or staging of media fails for a reason
//     a redelivery may fix, or the window state cannot be written, and for
//     a retryable sink failure; 422 for any other sink failure;
//   - 200 with empty TwiML once the sink accepted the event, new or
//     duplicate.
//
// An incoming message's media are staged through the engine's attachment
// sink (adapter.AttachmentSink) before the window moves and Accept runs; see
// stageMedia.
//
// A request carrying MessageStatus is a status callback: a status envelope
// whose PayloadDelivery data reports the mapped delivery state and, when
// the callback URL carries them, the operation and attempt Send tagged it
// with (also as model.EchoRef), and whose delivery report
// (Envelope.Delivery) the engine settles that attempt from. Any other
// request is an incoming message,
// which first moves the sender's customer service window forward.
func (i *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			status(w, http.StatusMethodNotAllowed)
			return
		}
		if i.isClosed() || i.cfg.WebhookURL == "" {
			status(w, http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			status(w, http.StatusRequestEntityTooLarge)
			return
		case err != nil:
			status(w, http.StatusBadRequest)
			return
		}
		token, err := i.webhookToken(r)
		if err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
		signed := i.cfg.WebhookURL
		if r.URL.RawQuery != "" {
			signed += "?" + r.URL.RawQuery
		}
		form, err := twilio.Verify(token, signed, r.Header.Get("Content-Type"), body, r.Header.Get(twilio.SignatureHeader))
		switch {
		case errors.Is(err, twilio.ErrMalformed):
			status(w, http.StatusBadRequest)
			return
		case err != nil:
			status(w, http.StatusForbidden)
			return
		}
		if form.Get("AccountSid") != i.cfg.AccountSID {
			status(w, http.StatusForbidden)
			return
		}
		in, ok := i.ingress(form, r.URL.Query())
		if !ok {
			status(w, http.StatusBadRequest)
			return
		}
		if err := i.stageMedia(r.Context(), sink, form, &in); err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
		if in.Envelope.Kind == model.EnvelopeMessage {
			if err := i.recordInbound(r.Context(), in.Envelope.Sender.ID, in.Envelope.OccurredAt); err != nil {
				status(w, http.StatusServiceUnavailable)
				return
			}
		}
		if _, err := sink.Accept(r.Context(), in); err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
				status(w, http.StatusServiceUnavailable)
				return
			}
			status(w, http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, emptyTwiML)
	})
}

func (i *Instance) webhookToken(r *http.Request) ([]byte, error) {
	c, err := i.resolve(r.Context(), PurposeWebhook, ports.KindSecret, ports.KindPassword)
	if err != nil {
		return nil, err
	}
	switch c := c.(type) {
	case ports.Secret:
		if len(c.Bytes) > 0 {
			return c.Bytes, nil
		}
	case ports.Password:
		if len(c.Secret) > 0 {
			return c.Secret, nil
		}
	}
	return nil, errors.New("no webhook auth token")
}

// address strips the WhatsApp prefix from a Twilio address. ok is false
// for any other address, such as an SMS number.
func address(s string) (string, bool) {
	a, ok := strings.CutPrefix(s, AddressPrefix)
	return a, ok && a != ""
}

// ingress normalizes a verified form. ok is false when it lacks the fields
// its kind needs.
func (i *Instance) ingress(form, query url.Values) (model.Ingress, bool) {
	sid := form.Get("MessageSid")
	from, okFrom := address(form.Get("From"))
	to, okTo := address(form.Get("To"))
	if sid == "" || !okFrom || !okTo {
		return model.Ingress{}, false
	}
	ch := i.channel.Ref
	now := i.clock.Now().UTC()
	if st := form.Get("MessageStatus"); st != "" {
		d := delivery(st, form.Get("ErrorCode"), query)
		data, err := json.Marshal(d)
		if err != nil {
			return model.Ingress{}, false
		}
		refs := []model.ExternalRef{model.ProviderMessageRef(ch, sid)}
		if d.Operation != "" && d.Attempt != "" {
			refs = append(refs, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: d.Operation}, d.Attempt))
		}
		return model.Ingress{
			Call: model.Call{Correlation: model.CorrelationID("twilio-whatsapp-status-" + sid)},
			Envelope: model.Envelope{
				// One event per reported status: redeliveries of a status
				// deduplicate, later statuses do not.
				EventID: sid + ":" + st, MessageID: sid, Kind: model.EnvelopeStatus,
				// Status callbacks report the sending address as From.
				Destination: from, Assurance: "verified",
				References: refs,
				OccurredAt: now,
				Payload:    model.Extension{Name: PayloadDelivery, Version: 1, Data: data},
				Delivery:   twilio.Report(st, d.ErrorCode),
			},
		}, true
	}
	inbound, err := json.Marshal(Inbound{
		ProfileName: form.Get("ProfileName"), WaID: form.Get("WaId"), MessageType: form.Get("MessageType"),
		RepliedTo:      form.Get("OriginalRepliedMessageSid"),
		ExternalUserID: strings.TrimPrefix(form.Get("ExternalUserId"), AddressPrefix),
	})
	if err != nil {
		return model.Ingress{}, false
	}
	return model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("twilio-whatsapp-message-" + sid)},
		Envelope: model.Envelope{
			EventID: sid, MessageID: sid, InReplyTo: form.Get("OriginalRepliedMessageSid"), Destination: to, Kind: model.EnvelopeMessage,
			Sender:           model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: RouteKind, ID: from},
			Assurance:        "verified",
			SenderAttributes: senderAttributes(from, form),
			Content:          content(form),
			Audience:         model.Audience{Visibility: model.Public},
			OccurredAt:       now,
			Payload:          model.Extension{Name: PayloadInbound, Version: 1, Data: inbound},
		},
	}, true
}

// senderAttributes are the attributes WhatsApp reports for the sender of a
// verified incoming message, for the engine to record on the sender's
// identity when it first sees it: the phone number (From when it is a
// number, otherwise WaId when Twilio sends one) and the business-scoped
// user ID (ExternalUserId; From itself when it is an ID and Twilio sent no
// ExternalUserId). A value that is not a valid attribute is left out and
// never fails the message.
func senderAttributes(from string, form url.Values) []model.Attribute {
	var out []model.Attribute
	add := func(kind model.AttributeKind, value string) {
		if a := (model.Attribute{Kind: kind, Value: value, Verified: true}); value != "" && a.Validate() == nil {
			out = append(out, a)
		}
	}
	if strings.HasPrefix(from, "+") {
		add(model.AttributePhone, from)
	} else if wa := form.Get("WaId"); wa != "" {
		add(model.AttributePhone, "+"+wa)
	}
	id := strings.TrimPrefix(form.Get("ExternalUserId"), AddressPrefix)
	if id == "" && !strings.HasPrefix(from, "+") {
		id = from
	}
	add(model.AttributeWhatsAppUserID, id)
	return out
}

// Inbound is the PayloadInbound data of an incoming message. ProfileName
// and WaID are what WhatsApp reports, claims rather than identity.
type Inbound struct {
	ProfileName string `json:"profile_name,omitempty"`
	WaID        string `json:"wa_id,omitempty"`
	MessageType string `json:"message_type,omitempty"`
	// RepliedTo is the SID of the message this one replies to.
	RepliedTo string `json:"replied_to,omitempty"`
	// ExternalUserID is the WhatsApp business-scoped user ID Twilio reports
	// as ExternalUserId (CC.<id>, without the "whatsapp:" prefix). It names
	// the user whether or not a phone number is visible: equal to the
	// sender's ID when WhatsApp hides the number, the ID behind it when
	// From is the number. Empty when Twilio did not send it.
	ExternalUserID string `json:"external_user_id,omitempty"`
}

// Button is the PayloadButton data of a quick-reply button press.
type Button struct {
	Payload string `json:"payload,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Location is the PayloadLocation data of a shared location.
type Location struct {
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
	Address   string `json:"address,omitempty"`
	Label     string `json:"label,omitempty"`
}

// Media is the PayloadMedia data of an incoming message: how many media
// items Twilio received and their content types. Media URLs are not kept;
// the media themselves are staged as attachment parts.
type Media struct {
	Count        int      `json:"count"`
	ContentTypes []string `json:"content_types,omitempty"`
	// Unavailable lists the media whose download failed for good, with
	// the reason (twilio.GapGone, GapUnauthorized, GapRedirect or
	// GapFailed). A media that is neither staged nor listed was not tried:
	// the engine refused it, the host gave no blob store, or its URL is
	// off the account's Media resources.
	Unavailable []twilio.MediaGap `json:"unavailable,omitempty"`
}

// content maps an incoming message to parts: the Body as text, then a
// structured part for media, a button press or a location.
func content(form url.Values) model.Content {
	var c model.Content
	if b := form.Get("Body"); b != "" {
		c.Parts = append(c.Parts, model.Part{Kind: model.PartText, Text: b})
	}
	structured := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		c.Parts = append(c.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: name, Version: 1, Data: data}})
	}
	if n, err := strconv.Atoi(form.Get("NumMedia")); err == nil && n > 0 {
		m := Media{Count: n}
		for k := 0; k < n && k < maxMedia; k++ {
			if ct := form.Get("MediaContentType" + strconv.Itoa(k)); ct != "" {
				m.ContentTypes = append(m.ContentTypes, ct)
			}
		}
		structured(PayloadMedia, m)
	}
	if p, t := form.Get("ButtonPayload"), form.Get("ButtonText"); p != "" || t != "" {
		structured(PayloadButton, Button{Payload: p, Text: t})
	}
	if lat, lon := form.Get("Latitude"), form.Get("Longitude"); lat != "" && lon != "" {
		structured(PayloadLocation, Location{Latitude: lat, Longitude: lon, Address: form.Get("Address"), Label: form.Get("Label")})
	}
	return c
}

// Delivery is the PayloadDelivery data of a status callback.
type Delivery struct {
	// Status is Twilio's MessageStatus; State its delivery state, empty for
	// an undocumented status.
	Status string              `json:"status"`
	State  model.DeliveryState `json:"state,omitempty"`
	// ErrorCode is Twilio's ErrorCode, zero when absent; CodeOutsideWindow
	// marks a free-form message sent outside the customer service window.
	ErrorCode int `json:"error_code,omitempty"`
	// Operation and Attempt are the IDs Send tagged the status callback
	// URL with.
	Operation model.ID `json:"operation,omitempty"`
	Attempt   model.ID `json:"attempt,omitempty"`
}

func delivery(st, code string, query url.Values) Delivery {
	d := Delivery{Status: st}
	d.State, _ = twilio.DeliveryState(st)
	if n, err := strconv.Atoi(code); err == nil && n > 0 {
		d.ErrorCode = n
	}
	if id, err := model.ParseID(query.Get(callbackOperation)); err == nil {
		d.Operation = id
	}
	if id, err := model.ParseID(query.Get(callbackAttempt)); err == nil {
		d.Attempt = id
	}
	return d
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
