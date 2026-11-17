package services

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// intentConversationLabels names the stored intent of a held
// AddLabels or RemoveLabels: the change an approval writes.
const intentConversationLabels = "uma.intent.conversation_labels"

// conversationLabelsIntent is the stored intent of a held label change.
type conversationLabelsIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	Labels       []model.LabelRef      `json:"labels,omitempty"`
	Add          bool                  `json:"add"`
	Correlation  model.CorrelationID   `json:"correlation,omitempty"`
}

// holdLabels stores a label change as held for a human decision, claiming
// its key: the conversation is written only when the hold is approved, by
// applyHeldLabels in the approval's unit. The claim checks what a label
// change checks, so a proposal that could never apply is refused up front.
func holdLabels(ctx context.Context, c *engine.Client, req model.ConversationLabels, add bool) (model.Acceptance, error) {
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	data, err := json.Marshal(conversationLabelsIntent{
		Conversation: req.Conversation, Labels: req.Labels, Add: add, Correlation: req.Correlation,
	})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "labels cannot be stored")
	}
	res := req.Conversation.Ref()
	return operations.Submit(ctx, c, operations.Submission{
		Kind: model.ActionConversationLabel, Target: res, Owner: res, Command: req.Command,
		Audience:    model.Audience{Visibility: model.Internal},
		Fingerprint: conversationLabelsFingerprint(req, add),
		Intent:      model.Extension{Name: intentConversationLabels, Version: 1, Data: data},
		Local:       true,
		Result:      &res,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			_, err := checkLabelChange(ctx, tx, labelChange{conv: req.Conversation, labels: req.Labels, expected: req.ExpectedRevision})
			return err
		},
	})
}

func applyHeldLabels(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) error {
	var in conversationLabelsIntent
	if err := json.Unmarshal(intent.Data, &in); err != nil {
		return model.NewError(model.Validation, "args: not readable as this intent")
	}
	ids, err := allocate(ctx, e, len(in.Labels))
	if err != nil {
		return err
	}
	return writeLabelChange(ctx, tx, e, key.Caller, op.Ref, labelChange{
		conv: in.Conversation, labels: in.Labels, add: in.Add, correlation: in.Correlation, rev: rev, events: ids,
	})
}

// validateHeldLabelsEdit admits the edit of a held label change: the label
// set may change; the conversation, the direction and the correlation are
// the claim's.
func validateHeldLabelsEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	var in, old conversationLabelsIntent
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
	probe.Labels = old.Labels
	if !reflect.DeepEqual(probe, old) {
		return model.NewError(model.Validation, "args: only the labels can change")
	}
	if n := len(in.Labels); n == 0 || n > model.MaxConversationLabels {
		return model.NewError(model.Validation, "args: labels: required, at most "+strconv.Itoa(model.MaxConversationLabels))
	}
	seen := make(map[model.LabelRef]bool, len(in.Labels))
	for _, l := range in.Labels {
		if err := l.Validate(); err != nil {
			return model.NewError(model.Validation, "args: labels: "+err.Error())
		}
		if l.Tenant != in.Conversation.Inbox.Tenant || seen[l] {
			return model.NewError(model.Validation, "args: labels must be distinct labels of the conversation's tenant")
		}
		seen[l] = true
	}
	return nil
}
