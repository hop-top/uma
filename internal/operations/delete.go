package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// DeleteIntent is the durable intent of a delete: the shared mutation part
// only.
type DeleteIntent struct {
	MutationIntent
}

// Delete queues the deletion of a message at the provider, under
// model.CapabilityDelete and model.ActionMessageDelete, with Edit's
// target, authorization, precheck and result rules. Once applied, the
// message becomes a tombstone; until then it is untouched. It is no
// privacy redaction.
func Delete(ctx context.Context, c *engine.Client, req model.DeleteMessage) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	return submitMutation(ctx, c, mutation{
		kind: model.ActionMessageDelete, capability: model.CapabilityDelete,
		message: req.Message, command: req.Command, compareRev: true,
		fingerprint: func(model.Audience) []byte { return deleteFingerprint(req) },
		encode: func(m MutationIntent) (model.Extension, error) {
			return encodeIntent(IntentDelete, DeleteIntent{MutationIntent: m})
		},
	})
}

// deleteFingerprint returns the version 1 fingerprint of a delete: kind,
// target message and expected revision.
func deleteFingerprint(req model.DeleteMessage) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionMessageDelete))
	f.Set("target", Field().Ref(req.Message.Ref()))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// DecodeDeleteIntent reads a stored delete intent.
func DecodeDeleteIntent(e model.Extension) (DeleteIntent, error) {
	return decodeIntent[DeleteIntent](e, IntentDelete)
}
