package telegram

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Handler returns the Telegram-facing handler: the URL given to setWebhook.
// It reads the body and verifies the secret token (HeaderSecretToken)
// before anything else, and only then maps the update to ingress for sink.
// It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed request;
//   - 401 for a missing, wrong or repeated secret token, and 503 when the
//     secret cannot be resolved; nothing reaches the sink then;
//   - 200 once the sink accepted every envelope of the update, new or
//     duplicate, and for updates the adapter does not consume;
//   - 503 for a retryable sink failure and 422 for any other.
//
// Telegram repeats a request that is not answered 2XX "and give[s] up after
// a reasonable amount of attempts" (https://core.telegram.org/bots/api#setwebhook),
// and has no way to say a failure is final: a refusal that retrying cannot
// cure (an inbox bound to another bot, a conversation the selection cannot
// place) is answered 422 and is repeated a few times all the same; it shows
// in getWebhookInfo as last_error_message. Response bodies carry the status
// text only, and the 200 is empty. Telegram's rule that a webhook can answer
// with a method call is not used: the adapter never does, so a request's
// success is never unknown to it, and a callback query's progress bar is
// cleared by the response to the interaction, not by the webhook's answer.
//
// Under Config.Receive ReceivePoll the channel has no webhook: the handler
// answers every request 404 and reads nothing. The engine never mounts it,
// as the instance reports receive_http disabled.
func (x *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !x.cfg.offers(model.CapabilityReceiveHTTP) {
			reply(w, http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			reply(w, http.StatusMethodNotAllowed)
			return
		}
		if x.isClosed() || x.verifier == nil {
			reply(w, http.StatusServiceUnavailable)
			return
		}
		body, _, err := x.verifier.VerifyHTTP(r)
		if err != nil {
			x.verifier.Reject(w, err)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			reply(w, http.StatusBadRequest)
			return
		}
		var u update
		if err := json.Unmarshal(body, &u); err != nil || u.UpdateID <= 0 {
			reply(w, http.StatusBadRequest)
			return
		}
		batch, err := x.normalize(u)
		if err != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		x.deliver(r.Context(), w, sink, batch)
	})
}

// delivery is one envelope of an update, with the envelope to record
// instead when the first targets a message the sink does not know.
type delivery struct {
	ingress  model.Ingress
	fallback *model.Ingress
}

// deliver hands each envelope to sink in order and answers.
//
// An edit of a message the sink does not know records the message instead
// (its first observation). A reaction on a message the sink does not hold
// is dropped: there is nothing to apply it to. Telegram reports no
// deletion of a message, so none is handled here.
func (x *Instance) deliver(ctx context.Context, w http.ResponseWriter, sink adapter.Sink, batch []delivery) {
	for _, d := range batch {
		_, err := sink.Accept(ctx, d.ingress)
		if model.IsCode(err, model.NotFound) && d.fallback != nil {
			_, err = sink.Accept(ctx, *d.fallback)
		}
		if model.IsCode(err, model.NotFound) && dropsWhenUnknown(d.ingress.Envelope.Kind) {
			err = nil
		}
		if err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
				reply(w, http.StatusServiceUnavailable)
				return
			}
			reply(w, http.StatusUnprocessableEntity)
			return
		}
	}
	// An empty 200: a webhook may answer with a method call as its body, and
	// the adapter never does, so there is nothing to say.
	w.WriteHeader(http.StatusOK)
}

func dropsWhenUnknown(kind string) bool {
	return kind == model.EnvelopeReactionAdded || kind == model.EnvelopeReactionRemoved
}

func reply(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
