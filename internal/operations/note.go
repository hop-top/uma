package operations

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// IntentNote names the stored intent of an internal note delivered to the
// provider.
const IntentNote = "uma.intent.note"

// NoteIntent is the durable intent of a delivered internal note. As for a
// reply, the route is read from the conversation at execution.
type NoteIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	Content      model.Content         `json:"content"`
	Audience     model.Audience        `json:"audience"`
	// Correlation is the call's correlation, carried so a held note is
	// written, on approval, as the note would have been.
	Correlation model.CorrelationID `json:"correlation,omitempty"`
}

// DeliveredNote is an internal note to queue for the provider: the
// request, the channel type of its conversation, its internal audience and
// the note fingerprint the local path uses too, so a retry replays
// whichever path stored it.
type DeliveredNote struct {
	Request     model.InternalNote
	Source      model.TypeKey
	Audience    model.Audience
	Fingerprint []byte
}

// Note queues an internal note for delivery as a private provider note in
// an inbox that opted in (model.Inbox.DeliverNotes). It returns once the
// operation, the note message with its message.created event (origin
// note) and its pending work commit, with state model.Queued; Runtime.Run
// delivers it through the adapter's adapter.NoteSender. The channel's
// adapter must declare model.CapabilityInternalNote.
func Note(ctx context.Context, c *engine.Client, n DeliveredNote) (model.Acceptance, error) {
	if err := outboundCapability(c.Engine(), n.Source, model.CapabilityInternalNote); err != nil {
		return model.Acceptance{}, err
	}
	req := n.Request
	intent := NoteIntent{Conversation: req.Conversation, Content: req.Content.Clone(), Audience: n.Audience, Correlation: req.Correlation}
	data, err := json.Marshal(intent)
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "note content cannot be stored")
	}
	out, err := newOutbound(ctx, c, req.Conversation, req.Correlation, n.Source, outboundMessage{
		content: intent.Content, audience: intent.Audience, origin: model.MessageOriginNote,
	})
	if err != nil {
		return model.Acceptance{}, err
	}
	result := out.ref.Ref()
	conv := req.Conversation.Ref()
	expected := req.ExpectedRevision
	return submit(ctx, c, submission{
		kind:        model.ActionMessageNote,
		target:      conv,
		owner:       conv,
		command:     req.Command,
		audience:    intent.Audience,
		fingerprint: n.Fingerprint,
		intent:      model.Extension{Name: IntentNote, Version: 1, Data: data},
		workKind:    WorkKindOperation,
		precheck: func(ctx context.Context, tx store.Tx) error {
			if expected == "" {
				return nil
			}
			cur, err := tx.GetConversation(ctx, req.Conversation)
			if err != nil {
				return err
			}
			if cur.Revision != expected {
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			return nil
		},
		apply:         out.write,
		result:        &result,
		resultMessage: &out.ref,
	})
}

// DecodeNoteIntent reads a stored delivered-note intent.
func DecodeNoteIntent(e model.Extension) (NoteIntent, error) {
	var in NoteIntent
	if e.Name != IntentNote || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}
