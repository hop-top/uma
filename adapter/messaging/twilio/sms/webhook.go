package sms

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// emptyTwiML answers an incoming message without replying to it.
const emptyTwiML = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`

// maxMedia bounds the media entries read from one incoming message.
const maxMedia = twilio.MaxMedia

// Handler returns the handler Twilio calls for incoming messages and status
// callbacks, form-encoded POSTs or JSON ones. Before the sink sees anything
// it resolves the auth token (PurposeWebhook) and verifies
// X-Twilio-Signature over the configured WebhookURL plus the query the
// request arrived with and, for a form, every POST parameter. A JSON body is
// signed by the URL alone, which must carry the body's SHA-256 as bodySHA256
// (twilio.VerifyJSON); its top-level scalar fields are read under the form
// parameter names. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable, malformed or incomplete body;
//   - 403 for a missing or wrong signature or another account's request;
//   - 503 without a WebhookURL, when the instance is closed or the token
//     cannot be resolved, and for a retryable sink failure; 422 for any
//     other sink failure;
//   - 200 with empty TwiML once the sink accepted the event, new or
//     duplicate.
//
// An incoming MMS's media are staged through the engine's attachment sink
// (adapter.AttachmentSink) before Accept; see stageMedia.
//
// A request carrying MessageStatus is a status callback: a status envelope
// whose PayloadDelivery data reports the mapped delivery state and, when
// the callback URL carries them, the operation and attempt Send tagged it
// with (also as model.EchoRef), and whose delivery report
// (Envelope.Delivery) the engine settles that attempt from. Any other
// request is an incoming message from From to To.
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

// ingress normalizes a verified form. ok is false when it lacks the fields
// its kind needs.
func (i *Instance) ingress(form, query url.Values) (model.Ingress, bool) {
	sid, from, to := form.Get("MessageSid"), form.Get("From"), form.Get("To")
	if sid == "" || from == "" || to == "" {
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
			Call: model.Call{Correlation: model.CorrelationID("twilio-status-" + sid)},
			Envelope: model.Envelope{
				// One event per reported status: redeliveries of a status
				// deduplicate, later statuses do not.
				EventID: sid + ":" + st, MessageID: sid, Kind: model.EnvelopeStatus,
				// Status callbacks report the sending number as From.
				Destination: from, Assurance: "verified",
				References: refs,
				OccurredAt: now,
				Payload:    model.Extension{Name: PayloadDelivery, Version: 1, Data: data},
				Delivery:   twilio.Report(st, d.ErrorCode),
			},
		}, true
	}
	env, ok := i.messageEnvelope(sid, from, to, form.Get("Body"), mediaOf(form), now)
	if !ok {
		return model.Ingress{}, false
	}
	return model.Ingress{
		Call:     model.Call{Correlation: model.CorrelationID("twilio-message-" + sid)},
		Envelope: env,
	}, true
}

// messageEnvelope is the envelope of the incoming message sid from from to
// to, with body and media, received at at. The webhook and the history
// reader report a message alike, so the engine records it once whichever
// way it arrived. ok is false when the media summary cannot be encoded.
func (i *Instance) messageEnvelope(sid, from, to, body string, m *Media, at time.Time) (model.Envelope, bool) {
	var content model.Content
	if body != "" {
		content.Parts = append(content.Parts, model.Part{Kind: model.PartText, Text: body})
	}
	if m != nil {
		data, err := json.Marshal(m)
		if err != nil {
			return model.Envelope{}, false
		}
		content.Parts = append(content.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: PayloadMedia, Version: 1, Data: data}})
	}
	return model.Envelope{
		EventID: sid, MessageID: sid, Destination: to, Kind: model.EnvelopeMessage,
		Sender:     model.ExternalRef{Channel: i.channel.Ref, Namespace: Namespace, Kind: RouteKind, ID: from},
		Assurance:  "verified",
		Content:    content,
		Audience:   model.Audience{Visibility: model.Public},
		OccurredAt: at,
	}, true
}

// Delivery is the PayloadDelivery data of a status callback.
type Delivery struct {
	// Status is Twilio's MessageStatus; State its delivery state, empty for
	// an undocumented status.
	Status string              `json:"status"`
	State  model.DeliveryState `json:"state,omitempty"`
	// ErrorCode is Twilio's ErrorCode, zero when absent.
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

// mediaOf is the media summary of an incoming message's form, nil without
// media.
func mediaOf(form url.Values) *Media {
	n, err := strconv.Atoi(form.Get("NumMedia"))
	if err != nil || n <= 0 {
		return nil
	}
	m := Media{Count: n}
	for k := 0; k < n && k < maxMedia; k++ {
		if ct := form.Get("MediaContentType" + strconv.Itoa(k)); ct != "" {
			m.ContentTypes = append(m.ContentTypes, ct)
		}
	}
	return &m
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
