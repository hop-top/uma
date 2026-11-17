package jira

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Webhook headers Jira sends with a delivery.
const (
	// HeaderSignature carries "method=signature": the hex HMAC of the raw
	// body under the webhook's secret. Only method sha256 is accepted.
	HeaderSignature = "X-Hub-Signature"
	// HeaderIdentifier identifies the webhook; it is the same across
	// retries of one delivery. It is unsigned: kept as correlation only.
	HeaderIdentifier = "X-Atlassian-Webhook-Identifier"
)

// Handler returns the Jira-facing webhook handler: the receiver of an admin
// webhook (Jira administration, or the REST API under /rest/webhooks/1.0)
// registered with a secret. It reads the body and verifies X-Hub-Signature
// in constant time before anything else, and only then maps the delivery to
// ingress for sink. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed delivery;
//   - 401 for a missing, non-sha256 or wrong signature, and 503 when the
//     webhook secret cannot be resolved; nothing reaches the sink then;
//   - 200 once the sink accepted every envelope of the delivery, new or
//     duplicate, and for events the adapter does not consume;
//   - 503 for a retryable sink failure and 422 for any other.
//
// Jira retries a delivery that is not answered 200 only for 408, 409, 425,
// 429 and 5xx, so a refusal that retrying cannot fix (400, 401, 422) is
// final.
//
// The event ID is a digest of the signed body (eventID), never a header: the
// signature covers the body only, so any header is whatever the sender of a
// captured body writes. HeaderIdentifier is kept as the correlation.
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
		batch, err := x.normalize(eventID(body), r.Header.Get(HeaderIdentifier), body)
		if err != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		reply(w, x.deliver(r.Context(), sink, batch))
	})
}

// eventID is the deduplication identity of a delivery: the SHA-256 of the
// raw body X-Hub-Signature signed. A retry resends the body, so it is the
// same event; a different body is a different event under any header.
func eventID(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verified reports whether sig, "method=signature", is the HMAC of body
// under secret. Only method sha256 is accepted ("Jira might start using
// another method": an unknown one is refused, not guessed), never without a
// secret, and the comparison is constant time.
func verified(sig string, body, secret []byte) bool {
	method, hexsig, ok := strings.Cut(sig, "=")
	if !ok || method != "sha256" || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(hexsig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
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

// deliver hands each envelope to sink in order and returns the status to
// answer. An edit of a message the sink does not know records the message
// instead (its first observation); a deletion of one is dropped.
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
