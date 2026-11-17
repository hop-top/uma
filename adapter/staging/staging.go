// Package staging is the part of staging inbound bytes through the engine's
// attachment sink that every adapter shares: streaming one body into the
// sink, telling a failure a redelivery may fix from one that only costs the
// attachment, and deriving an opaque staging key. It is shared by adapters
// in several modules, so it imports the standard library and uma's public
// contract packages only.
//
// What to stage stays with each adapter: where the bytes come from, which
// of them qualify (caps, origins, credentials), the staging key of a source
// that has no opaque identifier of its own, the attachment's metadata
// defaults, and what a failed attempt does to the rest of the message.
package staging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Bounds on the attachments one message stages, for the sources that carry
// the bytes of a message's attachments with it (mail). The engine's own
// attachment size limit and media type checks apply on top.
const (
	// MaxAttachments is the number of attachments staged per message; the
	// ones after it are described only.
	MaxAttachments = 10
	// MaxAttachmentBytes is the decoded size of the largest attachment
	// staged; larger ones are described only.
	MaxAttachmentBytes = 25 << 20
)

// Outcome is how one staging attempt ended.
type Outcome int

const (
	// Staged: the attachment is in the sink's inbox.
	Staged Outcome = iota
	// Skipped: the engine refused the attachment for good (its checks, or
	// other bytes under the same key). Staging it again will not help; the
	// rest of the message is unaffected.
	Skipped
	// Disabled: the sink has no blob store. That holds for every
	// attachment, so an adapter may stop trying.
	Disabled
	// Retry: a redelivery may fix the failure. Stage returns its error.
	Retry
)

// Stage streams body into sink under req and reports how it ended. It
// returns an error only for Retry: the sink was unavailable, or ctx ended.
// The reference is set only for Staged. Staging is idempotent per channel
// and req.Key, so a redelivery restages the same attachment.
func Stage(ctx context.Context, sink adapter.AttachmentSink, req adapter.StageRequest, body io.Reader) (model.AttachmentRef, Outcome, error) {
	ref, err := sink.StageAttachment(ctx, req, body)
	switch {
	case err == nil:
		return ref, Staged, nil
	case Retryable(err):
		return model.AttachmentRef{}, Retry, err
	case model.IsCode(err, model.Disabled):
		return model.AttachmentRef{}, Disabled, nil
	}
	return model.AttachmentRef{}, Skipped, nil
}

// Retryable reports a failure a redelivery may fix: ctx ended, or an error
// whose code advises retrying the same operation.
func Retryable(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		model.CodeOf(err).DefaultRetry() == model.RetrySameOperation)
}

// OpaqueKey returns prefix followed by the SHA-256 of parts, so that a
// staging key is bounded and opaque whatever the provider's identifiers
// look like (the engine accepts at most 255 bytes). Parts are separated by
// NUL, so ("ab", "c") and ("a", "bc") differ; give it every identifier the
// key must be unique per, such as the destination, the message and the
// position of the attachment in it.
func OpaqueKey(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])
}

// HistorySink is the sink a history reader hands the mapping it shares with
// its inbound path: it stages through stager when one is given (a
// HistoryRequest's), and offers no staging otherwise, so the mapping
// describes attachments only. It accepts nothing and evaluates nothing: a
// history read delivers no ingress.
func HistorySink(stager adapter.AttachmentSink) adapter.Sink {
	if stager == nil {
		return noStaging{}
	}
	return stagingSink{stager}
}

type noStaging struct{}

func (noStaging) Accept(context.Context, model.Ingress) (model.IngressReceipt, error) {
	return model.IngressReceipt{}, model.NewError(model.Unsupported, "history read cannot accept")
}

func (noStaging) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "history read cannot evaluate")
}

type stagingSink struct{ adapter.AttachmentSink }

func (stagingSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	return noStaging{}.Accept(ctx, in)
}

func (stagingSink) EvaluateInteraction(ctx context.Context, in model.InteractionInput) (model.InteractionDecision, error) {
	return noStaging{}.EvaluateInteraction(ctx, in)
}
