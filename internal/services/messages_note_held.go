package services

import (
	"context"
	"encoding/json"
	"reflect"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// intentLocalNote names the stored intent of a held internal note that
// stays in uma: the note an approval writes.
const intentLocalNote = "uma.intent.note_local"

// localNoteIntent is the stored intent of a held local internal note.
type localNoteIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	Content      model.Content         `json:"content"`
	Audience     model.Audience        `json:"audience"`
	Correlation  model.CorrelationID   `json:"correlation,omitempty"`
}

// localNote is one local internal note, ready to write: the conversation,
// the message it becomes, its content and audience, the expected
// conversation revision, the message's revision and its event ID.
type localNote struct {
	conv        model.ConversationRef
	msg         model.MessageRef
	content     model.Content
	audience    model.Audience
	expected    model.Revision
	correlation model.CorrelationID
	rev         model.Revision
	event       model.ID
}

// writeLocalNote writes n in tx as actor, under op: the note message and
// its message.created event.
func writeLocalNote(ctx context.Context, tx store.Tx, e *engine.Engine, actor model.ActorID, op model.OperationRef, n localNote) error {
	var cur model.Conversation
	if err := loadConversation(ctx, tx, n.conv, &cur); err != nil {
		return err
	}
	if n.expected != "" && cur.Revision != n.expected {
		return model.NewError(model.Conflict, "conversation revision changed")
	}
	now := e.Clock().Now().UTC()
	note := model.Message{
		Ref: n.msg, Content: n.content, Audience: n.audience,
		OccurredAt: now, RecordedAt: now, Revision: n.rev,
		Origin: &model.Origin{Actor: actor, Operation: op},
	}
	if err := tx.PutMessage(ctx, note, ""); err != nil {
		return err
	}
	ev, err := commandEventAs(e, actor, n.event, model.EventMessageCreated, model.SchemaMessageCreated,
		n.msg.Ref(), note.Revision, n.audience, n.correlation,
		commandMessageCreated{Message: n.msg, Trigger: model.TriggerCommand, Origin: model.MessageOriginNote, Operation: op})
	if err != nil {
		return err
	}
	return tx.AppendEvents(ctx, []model.Event{ev})
}

// holdLocalNote stores an internal note that stays in uma as held, claiming
// its key and the message it will become; the note is written only when
// the hold is approved.
func holdLocalNote(ctx context.Context, c *engine.Client, req model.InternalNote, audience model.Audience, msg model.MessageRef) (model.Acceptance, error) {
	data, err := json.Marshal(localNoteIntent{
		Conversation: req.Conversation, Content: req.Content.Clone(), Audience: audience, Correlation: req.Correlation,
	})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "note content cannot be stored")
	}
	conv, result := req.Conversation.Ref(), msg.Ref()
	return operations.Submit(ctx, c, operations.Submission{
		Kind: model.ActionMessageNote, Target: conv, Owner: conv, Command: req.Command,
		Audience: audience, Fingerprint: noteFingerprint(req, audience),
		Intent: model.Extension{Name: intentLocalNote, Version: 1, Data: data},
		Local:  true, Result: &result, ResultMessage: &msg,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			var cur model.Conversation
			if err := loadConversation(ctx, tx, req.Conversation, &cur); err != nil {
				return err
			}
			if req.ExpectedRevision != "" && cur.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			return nil
		},
	})
}

func applyHeldLocalNote(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) error {
	var in localNoteIntent
	if err := json.Unmarshal(intent.Data, &in); err != nil {
		return model.NewError(model.Validation, "args: not readable as this intent")
	}
	if op.ResultMessage == nil {
		return model.NewError(model.Permanent, "held operation names no result message")
	}
	ids, err := allocate(ctx, e, 1)
	if err != nil {
		return err
	}
	return writeLocalNote(ctx, tx, e, key.Caller, op.Ref, localNote{
		conv: in.Conversation, msg: *op.ResultMessage, content: in.Content, audience: in.Audience,
		correlation: in.Correlation, rev: rev, event: ids[0],
	})
}

// validateHeldLocalNoteEdit admits the edit of a held local note: only the
// content may change.
func validateHeldLocalNoteEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	var in, old localNoteIntent
	if err := json.Unmarshal(edited.Data, &in); err != nil {
		return model.NewError(model.Validation, "args: not readable as this intent")
	}
	if err := json.Unmarshal(stored.Data, &old); err != nil {
		return err
	}
	if in.Conversation.Ref() != key.Target {
		return model.NewError(model.Validation, "args: conversation must be the operation's target")
	}
	probe := in
	probe.Content = old.Content
	if !reflect.DeepEqual(probe, old) {
		return model.NewError(model.Validation, "args: only the content can change")
	}
	if err := in.Content.ValidateIn(in.Conversation.Inbox); err != nil {
		return model.NewError(model.Validation, "args: "+err.Error())
	}
	return nil
}
