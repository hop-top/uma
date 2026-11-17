package httpmail

import (
	"context"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// stageFor stages the attachment bytes of m through the sink into the
// inbox of destination d and returns the attachment parts to add to its
// content. It marks the descriptions of the staged ones, on m's own copy of
// them: each destination stages and describes for itself.
//
// Only bytes the message carries are staged: those of a message/rfc822
// body, and those a JSON entry embeds. A URL in a JSON entry is a claim and
// is never fetched, whatever it names. A sink that cannot stage, or one
// without a blob store, stages nothing and the message is delivered with its
// attachments described. It returns an error only when a redelivery may fix
// it: the sink was unavailable, or ctx ended.
//
// The staging key is the event ID of the message at d, bound to the bytes
// of the request and the destination, so a redelivery restages the same
// attachments and another destination's are its own.
func stageFor(ctx context.Context, sink adapter.Sink, m *inbound, d destination) ([]model.Part, error) {
	if m.kept == nil {
		return nil, nil
	}
	kept := *m.kept
	kept.Attachments = slices.Clone(kept.Attachments)
	parts, err := stage.Stage(ctx, sink, stage.Request{
		Key: eventID(d.name, m.raw), Destination: d.name, Audience: model.Audience{Visibility: model.Public},
	}, &kept)
	if err != nil {
		return nil, err
	}
	m.attachments = slices.Clone(m.attachments)
	for n, a := range kept.Attachments {
		if a.Staged && n < len(m.attachments) {
			m.attachments[n].Staged = true
		}
	}
	return parts, nil
}
