package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// ReactIntent is the durable intent of a react operation: the shared
// mutation part, the emoji and whether it is removed.
type ReactIntent struct {
	MutationIntent
	Emoji  string `json:"emoji"`
	Remove bool   `json:"remove"`
}

// React queues adding the caller's reaction to a message at the provider,
// or removing it when Remove is set, under model.CapabilityReact and
// model.ActionMessageReact, with Edit's target, authorization, precheck
// and result rules, except that ExpectedRevision is optional and compared
// only when set.
func React(ctx context.Context, c *engine.Client, req model.Reaction) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	return submitMutation(ctx, c, mutation{
		kind: model.ActionMessageReact, capability: model.CapabilityReact,
		message: req.Message, command: req.Command, compareRev: req.ExpectedRevision != "",
		fingerprint: func(model.Audience) []byte { return reactFingerprint(req) },
		encode: func(m MutationIntent) (model.Extension, error) {
			return encodeIntent(IntentReact, ReactIntent{MutationIntent: m, Emoji: req.Emoji, Remove: req.Remove})
		},
	})
}

// reactFingerprint returns the version 1 fingerprint of a react operation:
// kind, target message, emoji, remove and expected revision.
func reactFingerprint(req model.Reaction) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionMessageReact))
	f.Set("target", Field().Ref(req.Message.Ref()))
	f.Set("emoji", Field().Str(req.Emoji))
	f.Set("remove", Field().Present(req.Remove))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// DecodeReactIntent reads a stored react intent.
func DecodeReactIntent(e model.Extension) (ReactIntent, error) {
	return decodeIntent[ReactIntent](e, IntentReact)
}
