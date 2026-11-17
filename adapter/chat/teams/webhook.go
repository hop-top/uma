package teams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// TeamsChannelID is the channelId of activities from Microsoft Teams.
const TeamsChannelID = "msteams"

// Handler returns the handler of the bot's messaging endpoint, where the
// Bot Connector service posts activities. It checks the request's token
// before it hands anything to sink (see authenticate), then maps the
// activity to ingress. It answers:
//
//   - 405 for a method other than POST, 413 for a body over the host's
//     limit, 400 for an unreadable or malformed activity;
//   - 401 for a missing or invalid token, 403 for a token whose key is not
//     endorsed for the activity's channel ("If the endorsement is missing,
//     return HTTP 403"), and 503 when the signing keys cannot be read;
//     nothing reaches the sink then;
//   - 200 once the sink accepted every envelope of the activity, new or
//     duplicate, and for activities the adapter does not consume or that
//     name another tenant or channel; an Action.Execute invoke gets the
//     invoke response body the Universal Action Model asks for;
//   - 503 for a retryable sink failure and 422 for any other.
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
		auth := r.Header.Get("Authorization")
		if auth == "" {
			reply(w, http.StatusUnauthorized)
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
		var act activity
		if json.Unmarshal(body, &act) != nil || act.Type == "" {
			reply(w, http.StatusBadRequest)
			return
		}
		if err := x.authenticate(r.Context(), auth, act.ServiceURL, act.ChannelID); err != nil {
			switch {
			case model.IsCode(err, model.Unavailable):
				reply(w, http.StatusServiceUnavailable)
			case errors.Is(err, errNotEndorsed):
				reply(w, http.StatusForbidden)
			default:
				reply(w, http.StatusUnauthorized)
			}
			return
		}
		x.handle(r.Context(), sink, act).write(w)
	})
}

// verdict is what the adapter answers one activity with.
type verdict struct {
	status int
	// body is a JSON answer; nil answers the status text.
	body []byte
	// refusal is the sink's refusal the status reports, if any.
	refusal error
}

func statusAnswer(code int) verdict { return verdict{status: code} }

func (a verdict) write(w http.ResponseWriter) {
	if a.body == nil {
		reply(w, a.status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// handle maps one authenticated activity. Activities of another channel
// (the Bot Framework serves several besides Teams) or another tenant are
// acknowledged and dropped: not an error, not this channel's.
func (x *Instance) handle(ctx context.Context, sink adapter.Sink, act activity) verdict {
	if act.ChannelID != TeamsChannelID || act.tenant() != x.cfg.TenantID {
		return statusAnswer(http.StatusOK)
	}
	var batch []delivery
	var err error
	switch act.Type {
	case "message":
		batch, err = x.newMessage(act)
	case "messageUpdate":
		batch, err = x.edited(act)
	case "messageDelete":
		batch, err = x.deleted(act)
	case "messageReaction":
		batch, err = x.reactions(act)
	case "invoke":
		return x.invoke(ctx, sink, act)
	}
	if err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	return x.deliver(ctx, sink, batch, statusAnswer(http.StatusOK))
}

// delivery is one envelope of an activity, with the envelope to record
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
			return verdict{status: http.StatusUnprocessableEntity, refusal: err}
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
