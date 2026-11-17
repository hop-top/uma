package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Headers of a signed interaction request
// (https://docs.discord.com/developers/interactions/overview).
const (
	HeaderSignature = "X-Signature-Ed25519"
	HeaderTimestamp = "X-Signature-Timestamp"
)

// verifier checks the Ed25519 signature Discord puts on every request to
// the Interactions Endpoint URL: the hex signature in X-Signature-Ed25519,
// over the X-Signature-Timestamp value immediately followed by the raw
// body, against the application's public key, hex, resolved through the
// credential port on every request. The timestamp, Unix seconds, must be
// within the replay window either way. The shared webhookauth package does
// the check (crypto/ed25519 underneath).
func (x *Instance) verifier() (*webhookauth.Verifier, error) {
	var src webhookauth.SecretSource = webhookauth.SecretFunc(func(context.Context) ([][]byte, error) { return nil, webhookauth.ErrUnavailable })
	if x.creds != nil {
		src = webhookauth.FromCredentials(x.creds, ports.CredentialRequest{
			Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeInteractions,
			Accept: []ports.CredentialKind{ports.KindSecret},
		})
	}
	return webhookauth.New(webhookauth.Config{
		Scheme: webhookauth.SchemeEd25519, Header: HeaderSignature, Encoding: webhookauth.EncodingHex,
		Payload: "{timestamp}{body}", KeyEncoding: webhookauth.EncodingHex,
		Timestamp: webhookauth.Timestamp{Header: HeaderTimestamp, Format: webhookauth.TimestampUnix, Tolerance: x.cfg.tolerance()},
	}, src, webhookauth.Deps{Clock: clockFunc(x.now)})
}

type clockFunc func() time.Time

func (c clockFunc) Now() time.Time { return c() }

// Handler returns the Interactions Endpoint URL handler. It reads the body
// and verifies the request's signature and timestamp before anything else,
// then answers:
//
//   - 404 when the channel receives over the gateway only, reading nothing;
//     405 for a method other than POST, 503 once closed, 413 for a body over
//     the host's limit, 400 for an unreadable one;
//   - 401 for a missing, malformed, wrong or stale signature ("If the
//     signature fails, your app should respond with a 401"; Discord sends
//     such requests itself and removes a URL that accepts them), 503 when
//     the public key cannot be resolved;
//   - 200 {"type":1} to a PING;
//   - 200 {"type":8,"data":{"choices":[]}} to an autocomplete request,
//     recording nothing;
//   - for a command, a component or a modal submission, 200 with the
//     deferral (type 5, or 6 for a control on a message) once the sink
//     recorded the interaction, inside Discord's three seconds; the answer
//     itself follows through Respond. 503 for a retryable sink refusal and
//     422 for any other: Discord shows its user that the interaction failed;
//   - 400 for a payload the adapter cannot read or another application's.
func (x *Instance) Handler(sink adapter.Sink) http.Handler {
	v, verr := x.verifier()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !x.cfg.offers(model.CapabilityReceiveHTTP) {
			reply(w, http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			reply(w, http.StatusMethodNotAllowed)
			return
		}
		if x.isClosed() || verr != nil {
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
		res, err := v.Verify(r.Context(), webhookauth.Request{Header: r.Header, Body: body})
		if err != nil {
			if webhookauth.Status(err) == http.StatusServiceUnavailable {
				reply(w, http.StatusServiceUnavailable)
				return
			}
			reply(w, http.StatusUnauthorized)
			return
		}
		var p interaction
		if json.Unmarshal(body, &p) != nil {
			reply(w, http.StatusBadRequest)
			return
		}
		if p.Type == interactionPing {
			writeJSON(w, map[string]any{"type": callbackPong})
			return
		}
		typ, data, err := x.interactionAnswer(r.Context(), sink, p, res.Timestamp, "")
		if _, bad := err.(malformed); bad {
			reply(w, http.StatusBadRequest)
			return
		}
		if err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
				reply(w, http.StatusServiceUnavailable)
			} else {
				reply(w, http.StatusUnprocessableEntity)
			}
			return
		}
		out := map[string]any{"type": typ}
		if data != nil {
			out["data"] = data
		}
		writeJSON(w, out)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func reply(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
