package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// EditIntent is the durable intent of an edit: the shared mutation part
// and the replacement content.
type EditIntent struct {
	MutationIntent
	Content model.Content `json:"content"`
}

// Edit queues the replacement of a message's content at the provider. The
// channel's adapter must declare model.CapabilityEdit (else
// model.Unsupported); the caller is authorized for model.ActionMessageEdit
// on the message under its audience at ExpectedRevision. No uma origin is
// required: policy and the provider account decide which messages are
// editable. A new claim requires the message present, neither deleted nor
// redacted, at ExpectedRevision, and no live stream's message (each
// model.Conflict). The operation is model.Queued with the message as its
// result; the message itself is untouched until the edit applies.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts. The payload includes the
// message's audience at the call: a retry after the audience narrowed is a
// different command and conflicts.
func Edit(ctx context.Context, c *engine.Client, req model.EditMessage) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	content := req.Content.Clone()
	return submitMutation(ctx, c, mutation{
		kind: model.ActionMessageEdit, capability: model.CapabilityEdit,
		message: req.Message, command: req.Command, compareRev: true,
		fingerprint: func(a model.Audience) []byte { return editFingerprint(req, a) },
		encode: func(m MutationIntent) (model.Extension, error) {
			return encodeIntent(IntentEdit, EditIntent{MutationIntent: m, Content: content})
		},
	})
}

// editFingerprint returns the version 1 fingerprint of an edit: kind,
// target message, content, audience and expected revision. Correlation and
// the base snapshot are excluded.
func editFingerprint(req model.EditMessage, audience model.Audience) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionMessageEdit))
	f.Set("target", Field().Ref(req.Message.Ref()))
	f.Set("content", Field().Content(req.Content))
	f.Set("audience", Field().Audience(audience))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// DecodeEditIntent reads a stored edit intent.
func DecodeEditIntent(e model.Extension) (EditIntent, error) {
	return decodeIntent[EditIntent](e, IntentEdit)
}
