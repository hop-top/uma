package httpmail

import (
	"context"
	"errors"
	"io"
	stdmime "mime"
	"net/http"
	"slices"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// credentialRequest asks the host for the request credential of this
// channel, for PurposeWebhook.
func (i *Instance) credentialRequest() ports.CredentialRequest {
	return ports.CredentialRequest{Channel: i.ch.Ref, Reference: i.ch.CredentialRef, Purpose: PurposeWebhook}
}

// Handler returns the handler the host mounts for the channel (through
// Runtime.Webhook). It answers:
//
//   - 405 for a method other than POST;
//   - 401, 400, 409, 413 or 503 for a request the authentication refuses,
//     decided before the body is read for anything but its signature: see
//     package webhookauth;
//   - 415 for a content type no accepted format takes;
//   - 400 for a body that is not a message of its format, or has no sender
//     or no recipient;
//   - 422 for a message to no recipient of the channel, and for a refusal
//     of the sink that retrying cannot change;
//   - 503 for a retryable failure of the sink, and when the instance is
//     closed. The delivery is then not counted as seen: the sender's retry
//     is no replay;
//   - 200 with {"accepted": n} once the sink accepted the message for each
//     of its n destinations, new or duplicate. Hosted relays take only 200
//     as success.
//
// The adapter writes nothing but through the sink, and hands it nothing of
// a request that failed authentication.
func (i *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.serve(w, r, sink)
	})
}

func (i *Instance) serve(w http.ResponseWriter, r *http.Request, sink adapter.Sink) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		status(w, http.StatusMethodNotAllowed)
		return
	}
	if i.closed.Load() {
		status(w, http.StatusServiceUnavailable)
		return
	}
	body, res, err := i.verifier.VerifyHTTP(r)
	if err != nil {
		i.verifier.Reject(w, err)
		return
	}
	n, code := i.accept(r, body, sink)
	if code != http.StatusOK {
		// Nothing was applied that a retry would repeat: a delivery that
		// failed is not a replay of itself.
		_ = res.Release(context.WithoutCancel(r.Context()))
		status(w, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"accepted":`+strconv.Itoa(n)+`}`+"\n")
}

// accept decodes an authenticated body and hands it to the sink, once per
// destination, with the attachments it carries staged first (stageFor). It returns the number of destinations accepted and the
// status to answer.
func (i *Instance) accept(r *http.Request, body []byte, sink adapter.Sink) (int, int) {
	format, ok := i.format(r)
	if !ok {
		return 0, http.StatusUnsupportedMediaType
	}
	var (
		msg inbound
		err error
	)
	switch format {
	case FormatJSON:
		msg, err = i.decodeJSON(body)
	case FormatRFC822:
		msg, err = i.decodeRFC822(r, body)
	}
	if err != nil {
		return 0, statusOf(err)
	}
	dests := i.route(msg.to)
	if len(dests) == 0 {
		return 0, http.StatusUnprocessableEntity
	}
	for _, d := range dests {
		dm := msg // staging marks this destination's own copy of the descriptions
		parts, err := stageFor(r.Context(), sink, &dm, d)
		if err != nil {
			return 0, sinkStatus(err)
		}
		in, err := i.ingress(dm, d)
		if err != nil {
			return 0, http.StatusUnprocessableEntity
		}
		if len(parts) > 0 {
			in.Envelope.Content.Parts = append(in.Envelope.Content.Parts, parts...)
			if in.Envelope.Content.Validate() != nil {
				return 0, http.StatusUnprocessableEntity
			}
		}
		if _, err := sink.Accept(r.Context(), in); err != nil {
			return 0, sinkStatus(err)
		}
	}
	return len(dests), http.StatusOK
}

// format is the accepted body format the request's content type names.
func (i *Instance) format(r *http.Request) (string, bool) {
	mt, _, err := stdmime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return "", false
	}
	var f string
	switch mt {
	case "application/json":
		f = FormatJSON
	case "message/rfc822":
		f = FormatRFC822
	default:
		return "", false
	}
	return f, i.cfg.accepts(f)
}

// destination is one inbox destination a message is delivered to, with the
// recipient addresses that selected it.
type destination struct {
	name       string
	recipients []string
}

// route maps recipient addresses to destinations, each once, in order of
// first appearance. An address no recipient of the channel matches selects
// nothing.
func (i *Instance) route(addrs []string) []destination {
	var out []destination
	for _, a := range addrs {
		name, ok := i.cfg.destination(a)
		if !ok {
			continue
		}
		at := slices.IndexFunc(out, func(d destination) bool { return d.name == name })
		if at < 0 {
			out = append(out, destination{name: name})
			at = len(out) - 1
		}
		if !slices.Contains(out[at].recipients, a) {
			out[at].recipients = append(out[at].recipients, a)
		}
	}
	return out
}

// refusal is a request the adapter cannot accept, and the status that says
// so.
type refusal struct{ status int }

func (r refusal) Error() string { return http.StatusText(r.status) }

// malformed is a body that is no message of its format.
var malformed = refusal{http.StatusBadRequest}

func statusOf(err error) int {
	var r refusal
	if errors.As(err, &r) {
		return r.status
	}
	return http.StatusBadRequest
}

// sinkStatus maps a sink failure to the sender's retry: a refusal that
// retrying cannot change is 422 (403 for a denied principal), anything else
// 503, so the relay keeps the message.
func sinkStatus(err error) int {
	switch code := model.CodeOf(err); {
	case code == model.Unauthorized:
		return http.StatusForbidden
	case code == "" || code.DefaultRetry() == model.RetrySameOperation:
		return http.StatusServiceUnavailable
	}
	return http.StatusUnprocessableEntity
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
