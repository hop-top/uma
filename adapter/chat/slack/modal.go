package slack

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// MaxBlockIDBytes is the length Slack allows an input block's block_id
// (https://docs.slack.dev/reference/block-kit/blocks/input-block/): the key
// of a field error.
const MaxBlockIDBytes = 255

// Slack's answer to a modal submission is the body of the HTTP response to
// the request that carried it: "In every case, apps must return a required
// acknowledgment response back to the HTTP request that sent the payload"
// (https://docs.slack.dev/surfaces/modals/). An empty 200 closes the view;
// response_action "errors" keeps it open and shows a message under each
// input block named by its block_id. The answer can never come later, through
// the response_url, so the host's decision has to be asked for inside the
// request: with Config.EvaluateInteractions the adapter asks the engine's
// interaction validator (adapter.Sink.EvaluateInteraction) before it records
// the submission. A decision of field errors is the answer and records
// nothing, because the submission did not happen: Slack keeps the modal open
// and sends a new one on resubmission. A decision with none is followed by
// the ordinary record and empty 200.
//
// response_action update, push and clear need a view the validator's
// decision has no slot for, and clear is what an empty 200 does not do: they
// are not offered.

// errorsResponse is the body of response_action "errors".
type errorsResponse struct {
	ResponseAction string            `json:"response_action"`
	Errors         map[string]string `json:"errors"`
}

// evaluateSubmission asks for the host's decision on in, a submission. It
// reports whether the submission goes on to be recorded, and the answer when
// it does not.
func (x *Instance) evaluateSubmission(ctx context.Context, sink adapter.Sink, in model.InteractionInput) (verdict, bool) {
	// The validator reads the submission, never the credential to answer it.
	in = in.Clone()
	in.ResponseToken, in.ResponseExpiresAt = "", time.Time{}
	dec, err := sink.EvaluateInteraction(ctx, in)
	if err != nil {
		return evaluationFailed(err), false
	}
	if len(dec.FieldErrors) == 0 {
		return verdict{}, true
	}
	if !validFieldErrors(dec.FieldErrors) {
		return statusAnswer(http.StatusUnprocessableEntity), false
	}
	return jsonAnswer(errorsResponse{ResponseAction: "errors", Errors: dec.FieldErrors}), false
}

// validFieldErrors reports whether every key can be a block_id and every
// message can be shown. Slack states no limit for a message and no behavior
// for a key naming no block, so those are not checked.
func validFieldErrors(errs map[string]string) bool {
	for k, msg := range errs {
		if k == "" || len(k) > MaxBlockIDBytes || !utf8.ValidString(k) || strings.IndexFunc(k, unicode.IsControl) >= 0 {
			return false
		}
		if strings.TrimSpace(msg) == "" || !utf8.ValidString(msg) {
			return false
		}
	}
	return true
}

// evaluationFailed answers a request whose evaluation failed. Slack shows
// its user an error for any answer but a 200 in time, and the submission was
// not recorded: a denied one (no validator configured, or the host refuses
// the actor) is 403, an unavailable validator 503, a passed deadline 504 and
// any other refusal 422.
func evaluationFailed(err error) verdict {
	switch code := model.CodeOf(err); {
	case code == model.DeadlineExpired:
		return statusAnswer(http.StatusGatewayTimeout)
	case code == model.Unauthorized:
		return statusAnswer(http.StatusForbidden)
	case code.DefaultRetry() == model.RetrySameOperation:
		return statusAnswer(http.StatusServiceUnavailable)
	}
	return statusAnswer(http.StatusUnprocessableEntity)
}
