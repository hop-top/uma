package gitlab

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Webhook headers GitLab sends with a delivery. The token is sent only
// when the webhook has a secret token; the idempotency headers name one
// delivery across its retries.
const (
	HeaderToken       = "X-Gitlab-Token" // #nosec G101 -- header name, not a credential
	HeaderEvent       = "X-Gitlab-Event"
	HeaderIdempotency = "Idempotency-Key"
	HeaderWebhookID   = "Webhook-Id"
)

// Handler returns the GitLab-facing webhook handler. It reads the body and
// verifies X-Gitlab-Token against the webhook secret in constant time
// before anything else, and only then maps the delivery to ingress for
// sink. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed delivery;
//   - 401 for a missing or wrong token, and 503 when the webhook secret
//     cannot be resolved; nothing reaches the sink then;
//   - 200 once the sink accepted every envelope of the delivery, new or
//     duplicate, and for events the adapter does not consume;
//   - 503 for a retryable sink failure and 422 for any other.
//
// The event ID is a digest of the body (eventID): GitLab resends the same
// body on a retry, so it is one event, and a different body is a different
// event under any header. The delivery's idempotency key is kept as the
// correlation. X-Gitlab-Event must agree with the payload it names (400
// otherwise).
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
		if !verified(r.Header.Get(HeaderToken), secret) {
			reply(w, http.StatusUnauthorized)
			return
		}
		correlation := r.Header.Get(HeaderIdempotency)
		if correlation == "" {
			correlation = r.Header.Get(HeaderWebhookID)
		}
		batch, err := x.normalize(eventID(body), correlation, eventKind(r.Header.Get(HeaderEvent)), body)
		if err != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		reply(w, x.deliver(r.Context(), sink, batch))
	})
}

// eventID is the deduplication identity of a delivery: the SHA-256 of its
// raw body. A retry resends the body, so it is the same event.
func eventID(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verified reports whether token is the webhook secret, in constant time
// whatever the lengths: both sides are hashed first. Never without a
// secret, and never an empty token.
func verified(token string, secret []byte) bool {
	if len(secret) == 0 || token == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(token)), sha256.Sum256(secret)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// webhookSecret resolves the channel's webhook secret token.
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

// deliver hands each envelope to sink in order and returns the status to
// answer. An edit of a message the sink does not know records the message
// instead (its first observation).
func (x *Instance) deliver(ctx context.Context, sink adapter.Sink, batch []delivery) int {
	for _, d := range batch {
		_, err := sink.Accept(ctx, d.ingress)
		if model.IsCode(err, model.NotFound) && d.fallback != nil {
			_, err = sink.Accept(ctx, *d.fallback)
		}
		if model.IsCode(err, model.NotFound) && d.ingress.Envelope.Kind == model.EnvelopeMessageDeleted {
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
