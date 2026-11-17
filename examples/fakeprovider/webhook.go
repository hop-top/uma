package fakeprovider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// TypeWebhook is the type key of the webhook variant: it declares
// receive_http and implements adapter.HTTPReceiver only. It reports text
// messages and, with WebhookEvent.Interaction, structured interactions
// (interaction.received), all through the same signed handler.
const TypeWebhook model.TypeKey = "org.example.fake.webhook"

// Webhook signing, as the fake provider does it: the hex HMAC-SHA256 of the
// raw request body under the channel's signing secret, sent in
// SignatureHeader with the "sha256=" prefix. The instance resolves the
// secret for WebhookCredentialPurpose on every request.
const (
	SignatureHeader          = "X-Fake-Signature"
	WebhookCredentialPurpose = "org.example.fake.webhook"
)

var webhook = variant{
	typ: TypeWebhook, exchange: providerIDs, caps: []model.Capability{model.CapabilityReceiveHTTP},
	events: []string{model.EventMessageReceived, model.EventInteractionReceived},
	open:   func(i *instance) adapter.Instance { return &Webhook{i: i} },
}

// NewWebhook returns the factory of the webhook variant.
func NewWebhook() *Factory { return newFactory(webhook) }

// Sign returns the SignatureHeader value for body under secret.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// WebhookEvent is the fake provider's webhook body: one inbound text
// message, or, with Interaction set, one interaction its Sender (the actor)
// performed, whose provider interaction ID is EventID.
type WebhookEvent struct {
	EventID     string              `json:"event_id"`
	MessageID   string              `json:"message_id,omitempty"`
	Destination string              `json:"destination"`
	Sender      string              `json:"sender"`
	Text        string              `json:"text"`
	OccurredAt  time.Time           `json:"occurred_at"`
	Interaction *WebhookInteraction `json:"interaction,omitempty"`
}

// WebhookInteraction is what the fake provider reports about an
// interaction: its kind (a model.Interaction* value) and the identifiers
// and values that kind carries, the provider message the control sits on
// (Message, a provider message ID), the response token with its expiry, and
// the protocol deadline by which the provider needs its acknowledgement.
type WebhookInteraction struct {
	Kind           string          `json:"kind"`
	Action         string          `json:"action,omitempty"`
	Command        string          `json:"command,omitempty"`
	Arguments      string          `json:"arguments,omitempty"`
	Fields         model.Extension `json:"fields"`
	Message        string          `json:"message,omitempty"`
	Token          string          `json:"token,omitempty"`
	TokenExpiresAt time.Time       `json:"token_expires_at"`
	Deadline       time.Time       `json:"deadline"`
}

// Webhook is the webhook variant: receive_http only.
type Webhook struct{ i *instance }

// Capabilities reports receive_http.
func (x *Webhook) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent. A closed instance answers
// 503.
func (x *Webhook) Close(context.Context) error { return x.i.close() }

// Handler returns the provider-facing handler. It reads the body, verifies
// its signature in constant time before anything else, and only then maps
// the event to an ingress for sink. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed body;
//   - 401 for a missing or wrong signature, and 503 when the signing secret
//     cannot be resolved; nothing reaches the sink in either case;
//   - 200 once the sink accepted the event, new or duplicate, so the
//     provider stops redelivering; for an interaction, only once the
//     receipt is durable, the transport acknowledgement never getting ahead
//     of the durable acceptance (503 otherwise, and the provider
//     redelivers), and recorded in Script.Acks with the instant it was
//     given against the interaction's deadline;
//   - 503 for a retryable sink failure, so the provider redelivers, and 422
//     for any other.
//
// Response bodies carry the status text only.
func (x *Webhook) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			status(w, http.StatusMethodNotAllowed)
			return
		}
		if x.i.isClosed() {
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
		secret, err := x.secret(r.Context())
		if err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
		if !hmac.Equal([]byte(r.Header.Get(SignatureHeader)), []byte(Sign(secret, body))) {
			status(w, http.StatusUnauthorized)
			return
		}
		var ev WebhookEvent
		if err := json.Unmarshal(body, &ev); err != nil || ev.EventID == "" {
			status(w, http.StatusBadRequest)
			return
		}
		receipt, err := sink.Accept(r.Context(), x.ingress(ev))
		if err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
				status(w, http.StatusServiceUnavailable)
				return
			}
			status(w, http.StatusUnprocessableEntity)
			return
		}
		if ev.Interaction != nil {
			if !receipt.Durable {
				status(w, http.StatusServiceUnavailable)
				return
			}
			x.recordAck(ev)
		}
		status(w, http.StatusOK)
	})
}

// secret resolves the channel's signing secret.
func (x *Webhook) secret(ctx context.Context) ([]byte, error) {
	if x.i.creds == nil {
		return nil, model.NewError(model.Unavailable, "no credentials port")
	}
	c, err := x.i.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.i.channel.Ref, Reference: x.i.channel.CredentialRef, Purpose: WebhookCredentialPurpose,
		Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		return nil, err
	}
	s, ok := c.(ports.Secret)
	if !ok || len(s.Bytes) == 0 {
		return nil, model.NewError(model.Unavailable, "signing secret is not a secret")
	}
	return s.Bytes, nil
}

// recordAck notes the acknowledgement of an interaction, late when the
// engine's clock reads at or past the interaction's deadline.
func (x *Webhook) recordAck(ev WebhookEvent) {
	a := Ack{EventID: ev.EventID, Deadline: ev.Interaction.Deadline}
	if x.i.clock != nil {
		a.At = x.i.clock.Now()
		a.Late = !a.At.Before(a.Deadline)
	}
	x.i.script.acked(a)
}

func (x *Webhook) ingress(ev WebhookEvent) model.Ingress {
	ch := x.i.channel.Ref
	if ev.Interaction != nil {
		return x.interactionIngress(ev)
	}
	return model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("webhook-" + ev.EventID)},
		Envelope: model.Envelope{
			EventID: ev.EventID, MessageID: ev.MessageID, Destination: ev.Destination, Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: "user", ID: ev.Sender},
			Assurance: "verified",
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: ev.Text}}},
			Audience:  model.Audience{Visibility: model.Public}, OccurredAt: ev.OccurredAt,
		},
	}
}

// interactionIngress maps an interaction to an interaction envelope. The
// actor is the authenticated sender, never a field of the payload, and the
// interaction carries no content: nothing is flattened into text.
func (x *Webhook) interactionIngress(ev WebhookEvent) model.Ingress {
	ch := x.i.channel.Ref
	w := ev.Interaction
	actor := model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: "user", ID: ev.Sender}
	in := model.InteractionInput{
		ID: ev.EventID, Kind: w.Kind, Action: w.Action, Command: w.Command, Arguments: w.Arguments, Actor: actor,
		Fields: w.Fields.Clone(), ResponseToken: w.Token, ResponseExpiresAt: w.TokenExpiresAt, Deadline: w.Deadline,
	}
	if w.Message != "" {
		m := model.ProviderMessageRef(ch, w.Message)
		in.Message = &m
	}
	return model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("webhook-" + ev.EventID)},
		Envelope: model.Envelope{
			EventID: ev.EventID, Destination: ev.Destination, Kind: model.EnvelopeInteraction, Sender: actor,
			Assurance: "verified", Audience: model.Audience{Visibility: model.Public}, OccurredAt: ev.OccurredAt,
			Interaction: &in,
		},
	}
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

var _ adapter.HTTPReceiver = (*Webhook)(nil)
