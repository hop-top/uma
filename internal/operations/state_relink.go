package operations

import (
	"context"
	"slices"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// ReferenceMove is a provider message reference that changed: the message
// held under Old is held under New now.
type ReferenceMove struct {
	Old, New model.ExternalRef
}

// relinkLimit bounds the messages read per reference: a provider reference
// names one message of the inbox, so more is a conflict and skipped.
const relinkLimit = 8

// RelinkMessages adds the new provider reference of every move to the
// message of conv that holds the old one, directly ahead of it so a lookup
// of the message's provider reference finds the current one first, and
// keeps the old reference. A move whose old reference no message of conv
// holds, whose new reference the message holds already, or whose new
// reference another message holds, changes nothing, so it may run more
// than once. The changed messages move to the revision of op.
func RelinkMessages(ctx context.Context, tx store.Tx, op model.Operation, conv model.ConversationRef, moves []ReferenceMove) error {
	for _, mv := range moves {
		if mv.Old == mv.New || mv.Old.Channel != mv.New.Channel {
			continue
		}
		held, err := tx.MessagesByReference(ctx, conv.Inbox, mv.Old, relinkLimit)
		if model.IsCode(err, model.Conflict) {
			continue
		}
		if err != nil {
			return err
		}
		taken, err := tx.MessagesByReference(ctx, conv.Inbox, mv.New, 1)
		if err != nil && !model.IsCode(err, model.Conflict) {
			return err
		}
		if len(taken) > 0 || model.IsCode(err, model.Conflict) {
			continue
		}
		for _, m := range held {
			if m.Ref.Conversation != conv {
				continue
			}
			prev := m.Revision
			at := slices.Index(m.External, mv.Old)
			m.External = slices.Insert(slices.Clone(m.External), at, mv.New)
			m.Revision = op.Revision
			if err := tx.PutMessage(ctx, m, prev); err != nil {
				return err
			}
		}
	}
	return nil
}
