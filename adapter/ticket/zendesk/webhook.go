package zendesk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Webhook event types the handler maps (Zendesk ticket events, version
// 2022-11-06). Every other zen:event-type:ticket.* event is a work item
// change; non-ticket events are acknowledged and ignored.
const (
	eventPrefix            = "zen:event-type:ticket."
	eventCommentAdded      = eventPrefix + "comment_added"
	eventCommentPrivate    = eventPrefix + "comment_made_private"
	eventMerged            = eventPrefix + "merged"
	eventSoftDeleted       = eventPrefix + "soft_deleted"
	eventPermanentlyDelete = eventPrefix + "permanently_deleted"
	eventMarkedAsSpam      = eventPrefix + "marked_as_spam"
)

// webhookEvent is a Zendesk webhook event body.
type webhookEvent struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   string          `json:"time"`
	Detail ticket          `json:"detail"`
	Event  json.RawMessage `json:"event"`
}

// Handler returns the webhook handler for a Zendesk webhook subscribed to
// ticket events. It reads the body, verifies the signature and its
// timestamp before anything else, and only then normalizes the event for
// sink. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable body;
//   - 401 for a missing, malformed or wrong signature or a timestamp outside
//     the channel's tolerance, and 503 when the signing secret cannot be
//     resolved; nothing reaches the sink in either case;
//   - 400 for a verified body that is not a webhook event;
//   - 200 once the sink accepted the event, new or duplicate, and for
//     verified events the adapter ignores, so Zendesk stops redelivering;
//   - 503 for a retryable failure (the sink's or an echo lookup's), so
//     Zendesk redelivers, and 422 for any other.
func (i *Instance) Handler(sink adapter.Sink) http.Handler {
	x := i.x
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			status(w, http.StatusMethodNotAllowed)
			return
		}
		if x.isClosed() {
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
		secret, err := x.signingSecret(r.Context())
		if err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
		if err := verifySignature(r.Header, body, secret, x.now(), x.cfg.tolerance()); err != nil {
			status(w, http.StatusUnauthorized)
			return
		}
		var ev webhookEvent
		if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Type == "" {
			status(w, http.StatusBadRequest)
			return
		}
		in, ok, err := x.webhookIngress(r.Context(), ev)
		if err == nil && ok {
			_, err = sink.Accept(r.Context(), in)
		}
		if err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation || errors.Is(err, context.DeadlineExceeded) {
				status(w, http.StatusServiceUnavailable)
				return
			}
			status(w, http.StatusUnprocessableEntity)
			return
		}
		status(w, http.StatusOK)
	})
}

// webhookIngress normalizes a verified event; false means acknowledge and
// ignore.
func (x *instance) webhookIngress(ctx context.Context, ev webhookEvent) (model.Ingress, bool, error) {
	if !strings.HasPrefix(ev.Type, eventPrefix) || ev.Detail.ID == "" {
		return model.Ingress{}, false, nil
	}
	ch := x.channel.Ref
	at := parseTime(ev.Time)
	switch ev.Type {
	case eventCommentAdded:
		var e struct {
			Comment comment `json:"comment"`
		}
		if err := json.Unmarshal(ev.Event, &e); err != nil || e.Comment.ID == "" {
			return model.Ingress{}, false, model.NewError(model.Validation, "comment event without a comment")
		}
		staff := e.Comment.Author != nil && e.Comment.Author.IsStaff
		return x.commentIngress(ctx, commentIn{ticket: ev.Detail, comment: e.Comment, occurredAt: at, mayEcho: staff})
	case eventCommentPrivate:
		var e struct {
			Comment comment `json:"comment"`
		}
		if err := json.Unmarshal(ev.Event, &e); err != nil || e.Comment.ID == "" {
			return model.Ingress{}, false, model.NewError(model.Validation, "comment event without a comment")
		}
		// An audience-only edit: the message the comment was narrows to
		// restricted, keeping its content.
		return x.ingress(model.Envelope{
			EventID: ev.ID, MessageID: string(e.Comment.ID), Destination: x.destination(ev.Detail),
			Kind: model.EnvelopeMessageEdited, SystemActor: true, OccurredAt: at, Audience: audience(false),
		}), true, nil
	}
	binding, successor := model.WorkItemBindingActive, (*model.ExternalRef)(nil)
	switch ev.Type {
	case eventMerged:
		var e struct {
			Target zid `json:"target_ticket_id"`
		}
		if err := json.Unmarshal(ev.Event, &e); err != nil || e.Target == "" {
			return model.Ingress{}, false, model.NewError(model.Validation, "merge event without a target")
		}
		target := ticketRef(ch, e.Target)
		binding, successor = model.WorkItemBindingMerged, &target
	case eventSoftDeleted, eventPermanentlyDelete, eventMarkedAsSpam:
		binding = model.WorkItemBindingDeleted
	default:
		if strings.EqualFold(ev.Detail.Status, "deleted") {
			binding = model.WorkItemBindingDeleted
		}
	}
	return x.ingress(model.Envelope{
		EventID: ev.ID, Destination: x.destination(ev.Detail), Kind: model.EnvelopeWorkItem,
		References: []model.ExternalRef{ticketRef(ch, ev.Detail.ID)},
		OccurredAt: at, SourceRevision: ev.ID,
		WorkItem: x.state(ev.Detail, binding, successor),
	}), true, nil
}

// signingSecret resolves the channel's webhook signing secret.
func (x *instance) signingSecret(ctx context.Context) ([]byte, error) {
	if x.creds == nil {
		return nil, model.NewError(model.Unavailable, "no credentials port")
	}
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeWebhook,
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

func (x *instance) now() time.Time {
	if x.clock == nil {
		return time.Now()
	}
	return x.clock.Now()
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
