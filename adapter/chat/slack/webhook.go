package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Handler returns the Slack-facing handler. One Request URL serves the
// three kinds of request Slack sends the app, told apart by their content:
// Events API deliveries and url_verification (JSON), interactions
// (a form with a payload field) and slash commands (a form with a command
// field). It reads the body and verifies the request signature and its
// timestamp, in constant time, before anything else, and only then maps
// the request to ingress for sink. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed request;
//   - 401 for a missing, malformed, wrong or stale signature, and 503
//     when the signing secret cannot be resolved; nothing reaches the sink
//     then, and url_verification gets no challenge back;
//   - 200 once the sink accepted every envelope of the request, new or
//     duplicate, and for events and requests the adapter does not consume
//     or that name another team or app;
//   - 503 for a retryable sink failure and 422, with X-Slack-No-Retry, for
//     any other: Slack retries an event up to three times on a non-2xx
//     answer unless told not to
//     (https://docs.slack.dev/apis/events-api/).
//
// An interaction is acknowledged only after the sink recorded it, inside
// the three seconds Slack allows
// (https://docs.slack.dev/interactivity/handling-user-interaction/).
// Response bodies carry the status text only, apart from the challenge.
//
// Under Config.Receive ReceiveSocketMode the channel has no request URL:
// the handler answers every request 404 and reads nothing. The engine
// never mounts it, as the instance reports receive_http disabled.
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
		secret, err := x.secret(r.Context(), CredentialPurposeSigning)
		if err != nil {
			reply(w, http.StatusServiceUnavailable)
			return
		}
		if verifySignature(r.Header, body, secret, x.now(), x.cfg.tolerance()) != nil {
			reply(w, http.StatusUnauthorized)
			return
		}
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		switch {
		case err != nil:
			reply(w, http.StatusBadRequest)
		case mt == "application/json":
			x.event(r.Context(), sink, body).write(w)
		case mt == "application/x-www-form-urlencoded":
			x.form(r.Context(), sink, body, requestTime(r, x.now())).write(w)
		default:
			reply(w, http.StatusBadRequest)
		}
	})
}

// verdict is what the adapter makes of one delivery, whichever way it came:
// the response to a request at the request URL, and, over Socket Mode, what
// decides the envelope's acknowledgement (see socketAck). The request URL's
// rules are the reference; Socket Mode maps them.
type verdict struct {
	status int
	// contentType and body are the response body; a nil body is the status
	// text, an empty one no body at all.
	contentType string
	body        []byte
	// noRetry asks Slack not to deliver the request again
	// (X-Slack-No-Retry).
	noRetry bool
	// refusal is the sink's refusal the status reports, if any.
	refusal error
}

// statusAnswer answers code with its status text.
func statusAnswer(code int) verdict { return verdict{status: code} }

// emptyOK is the empty 200 Slack takes for an interaction or a command.
func emptyOK() verdict { return verdict{status: http.StatusOK, body: []byte{}} }

// jsonAnswer answers 200 with v as JSON.
func jsonAnswer(v any) verdict {
	b, err := json.Marshal(v)
	if err != nil {
		return statusAnswer(http.StatusInternalServerError)
	}
	return verdict{status: http.StatusOK, contentType: "application/json", body: b}
}

func (a verdict) write(w http.ResponseWriter) {
	if a.noRetry {
		w.Header().Set("X-Slack-No-Retry", "1")
	}
	if a.body == nil {
		reply(w, a.status)
		return
	}
	if a.contentType != "" {
		w.Header().Set("Content-Type", a.contentType)
	}
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// delivery is one envelope of a request, with the envelope to record
// instead when the first targets a message the sink does not know.
type delivery struct {
	ingress  model.Ingress
	fallback *model.Ingress
}

// deliver hands each envelope to sink in order and answers ok once every
// one is accepted.
//
// An edit of a message the sink does not know records the message instead
// (its first observation). A deletion or a reaction of a message the sink
// does not hold is dropped: there is nothing to apply it to.
func (x *Instance) deliver(ctx context.Context, sink adapter.Sink, batch []delivery, ok verdict) verdict {
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
				return verdict{status: http.StatusServiceUnavailable, refusal: err}
			}
			return verdict{status: http.StatusUnprocessableEntity, noRetry: true, refusal: err}
		}
	}
	return ok
}

func dropsWhenUnknown(kind string) bool {
	switch kind {
	case model.EnvelopeMessageDeleted, model.EnvelopeReactionAdded, model.EnvelopeReactionRemoved:
		return true
	}
	return false
}

func reply(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
