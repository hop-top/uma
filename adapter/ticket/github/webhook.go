package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Webhook headers GitHub sends with every delivery.
const (
	HeaderSignature = "X-Hub-Signature-256"
	HeaderDelivery  = "X-GitHub-Delivery"
	HeaderEvent     = "X-GitHub-Event"
)

// Handler returns the GitHub-facing webhook handler. It reads the body and
// verifies X-Hub-Signature-256 in constant time before anything else, and
// only then maps the delivery to ingress for sink. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed delivery;
//   - 401 for a missing, legacy or wrong signature, and 503 when the
//     webhook secret cannot be resolved; nothing reaches the sink then;
//   - 200 once the sink accepted every envelope of the delivery, new or
//     duplicate, and for events the adapter does not consume;
//   - 503 for a retryable sink failure and 422 for any other.
//
// The event ID is a digest of the signed body (eventID), never the delivery
// GUID: X-Hub-Signature-256 covers the body only, so the GUID is whatever the
// sender of a captured body writes. The GUID is kept as the correlation, and
// X-GitHub-Event must agree with the body it names (400 otherwise).
//
// Response bodies carry the status text only.
func (x *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			reply(w, http.StatusMethodNotAllowed)
			return
		}
		if x.isClosed() {
			reply(w, http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			reply(w, http.StatusRequestEntityTooLarge)
			return
		} else if err != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		secret, err := x.webhookSecret(r.Context())
		if err != nil {
			reply(w, http.StatusServiceUnavailable)
			return
		}
		if !verified(r.Header.Get(HeaderSignature), body, secret) {
			reply(w, http.StatusUnauthorized)
			return
		}
		delivery, event := r.Header.Get(HeaderDelivery), r.Header.Get(HeaderEvent)
		payload, ok := webhookPayload(r.Header.Get("Content-Type"), body)
		if !ok || delivery == "" || event == "" {
			reply(w, http.StatusBadRequest)
			return
		}
		batch, err := x.normalize(eventID(body), delivery, event, payload)
		if err != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		reply(w, x.deliver(r.Context(), sink, batch))
	})
}

// eventID is the deduplication identity of a delivery: the SHA-256 of the raw
// body X-Hub-Signature-256 signed. A redelivery resends the body, so it is
// the same event; a body replayed under another X-GitHub-Delivery is too, and
// a different body is a different event under any header. It spans the raw
// body, not the decoded payload, so a form-encoded delivery is identified by
// the exact bytes signed.
func eventID(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verified reports whether sig is the X-Hub-Signature-256 of body under
// secret. Only the sha256 form is accepted, and never without a secret.
func verified(sig string, body, secret []byte) bool {
	if len(secret) == 0 || !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	return gh.ValidateSignature(sig, body, secret) == nil
}

// webhookSecret resolves the channel's webhook secret.
func (x *Instance) webhookSecret(ctx context.Context) ([]byte, error) {
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
		return nil, model.NewError(model.Unavailable, "webhook secret is not a secret")
	}
	return s.Bytes, nil
}

// webhookPayload returns the JSON payload of a delivery body, sent as
// application/json or as the payload field of a form.
func webhookPayload(contentType string, body []byte) ([]byte, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, false
	}
	switch mt {
	case "application/json":
		return body, true
	case "application/x-www-form-urlencoded":
		form, err := url.ParseQuery(string(body))
		if err != nil || form.Get("payload") == "" {
			return nil, false
		}
		return []byte(form.Get("payload")), true
	}
	return nil, false
}

// deliver hands each envelope to sink in order and returns the status to
// answer. An edit of a message the sink does not know records the message
// instead (its first observation); a deletion of one, and a reaction
// summary, are dropped.
func (x *Instance) deliver(ctx context.Context, sink adapter.Sink, batch []delivery) int {
	for _, d := range batch {
		_, err := sink.Accept(ctx, d.ingress)
		if model.IsCode(err, model.NotFound) && d.fallback != nil {
			_, err = sink.Accept(ctx, *d.fallback)
		}
		if model.IsCode(err, model.NotFound) && (d.ingress.Envelope.Kind == model.EnvelopeMessageDeleted || d.ingress.Envelope.SystemActor) {
			err = nil
		}
		if err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
				return http.StatusServiceUnavailable
			}
			return http.StatusUnprocessableEntity
		}
	}
	return http.StatusOK
}

func reply(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
