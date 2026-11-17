package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Webhook headers Linear sends with every delivery
// (https://linear.app/developers/webhooks).
const (
	HeaderSignature = "Linear-Signature"
	HeaderDelivery  = "Linear-Delivery"
	HeaderEvent     = "Linear-Event"
)

// TimestampWindow is how far a delivery's webhookTimestamp may be from the
// adapter's clock: Linear recommends rejecting a delivery more than a
// minute off.
const TimestampWindow = time.Minute

// Handler returns the Linear-facing webhook handler. It reads the body and
// verifies Linear-Signature in constant time before anything else, and only
// then checks the replay window and maps the delivery to ingress for sink.
// It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed delivery;
//   - 401 for a missing or wrong signature and for a webhookTimestamp
//     outside TimestampWindow, and 503 when the webhook secret cannot be
//     resolved; nothing reaches the sink then;
//   - 200 once the sink accepted every envelope of the delivery, new or
//     duplicate, and for events the adapter does not consume;
//   - 503 for a retryable sink failure and 422 for any other.
//
// The event ID is a digest of the signed body, never Linear-Delivery: the
// signature covers the body only, so the delivery ID is whatever the sender
// of a captured body writes. It is kept as the correlation. Linear-Event
// must agree with the signed payload's type (400 otherwise).
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
		var h hook
		if json.Unmarshal(body, &h) != nil || h.Type == "" || h.WebhookTimestamp <= 0 {
			reply(w, http.StatusBadRequest)
			return
		}
		if !x.fresh(h.WebhookTimestamp) {
			reply(w, http.StatusUnauthorized)
			return
		}
		delivery := r.Header.Get(HeaderDelivery)
		if delivery == "" || r.Header.Get(HeaderEvent) != h.Type {
			reply(w, http.StatusBadRequest)
			return
		}
		batch, err := x.normalize(eventID(body), delivery, h)
		if err != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		reply(w, x.deliver(r.Context(), sink, batch))
	})
}

// fresh reports whether a webhookTimestamp (UTC epoch milliseconds) is within
// TimestampWindow of the clock.
func (x *Instance) fresh(ms float64) bool {
	if x.clock == nil || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return false
	}
	sent := time.UnixMilli(int64(ms))
	return x.clock.Now().Sub(sent).Abs() <= TimestampWindow
}

// eventID is the deduplication identity of a delivery: the SHA-256 of the
// raw body Linear-Signature signed. A redelivery that resends the body is
// the same event; a different body is a different event under any header.
func eventID(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verified reports whether sig is the hex HMAC-SHA256 of body under secret,
// compared in constant time, and never without a secret.
func verified(sig string, body, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// webhookSecret resolves the channel's webhook signing secret.
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
