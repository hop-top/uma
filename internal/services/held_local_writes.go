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

// Held local writes that Config.Held opts in: label configure (create or
// update), label delete and conversation transition. Each is stored as held
// when its command carries a Hold, writes nothing until approved, and is
// applied once by its applier in the approval's unit; a rejection or an
// expiry leaves nothing. A create reserves the label's ID when held, so the
// operation names the label it will write.

const (
	intentLabelConfigure         = "uma.intent.label_configure"
	intentLabelDelete            = "uma.intent.label_delete"
	intentConversationTransition = "uma.intent.conversation_transition"
)

func storedIntent(name string, v any) (model.Extension, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return model.Extension{}, model.NewError(model.Validation, "arguments cannot be stored")
	}
	return model.Extension{Name: name, Version: 1, Data: data}, nil
}

// readHeldIntent decodes the stored or edited arguments of a held write.
func readHeldIntent(e model.Extension, out any) error {
	if err := json.Unmarshal(e.Data, out); err != nil {
		return model.NewError(model.Validation, "args: not readable as this intent")
	}
	return nil
}

// --- label configure

type labelConfigureIntent struct {
	Label    model.Label    `json:"label"`
	Create   bool           `json:"create"`
	Expected model.Revision `json:"expected_revision"`
}

// holdLabelConfigure stores a label create or update as held, claiming its
// key. label already carries the ID a create will be written under.
func holdLabelConfigure(ctx context.Context, c *engine.Client, req model.ConfigureLabel, label model.Label, create bool, target model.Ref, result model.Ref) (model.Acceptance, error) {
	intent, err := storedIntent(intentLabelConfigure, labelConfigureIntent{Label: label, Create: create, Expected: req.ExpectedRevision})
	if err != nil {
		return model.Acceptance{}, err
	}
	return operations.Submit(ctx, c, operations.Submission{
		Kind: model.ActionLabelConfigure, Target: target, Owner: target, Command: req.Command,
		Fingerprint: labelFingerprint(req, target), Intent: intent, Local: true, Result: &result,
		HoldScope: labelHoldScope(label, create),
		Precheck: func(ctx context.Context, tx store.Tx) error {
			if create {
				return nil
			}
			stored, err := tx.GetLabel(ctx, label.Ref)
			if err := exactRef(err, stored.Ref == label.Ref); err != nil {
				return err
			}
			if req.ExpectedRevision != "" && stored.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "label revision changed")
			}
			return nil
		},
	})
}

// labelHoldScope is the duplicate-guard scope of a held label configure: for
// a create, the label's name under the key names are unique by, so another
// name is a distinct proposal; an update is scoped by its target alone.
func labelHoldScope(l model.Label, create bool) string {
	if !create {
		return ""
	}
	return model.LabelNameKey(l.Name)
}

func applyHeldLabelConfigure(ctx context.Context, _ *engine.Engine, tx store.Tx, _ model.Operation, _ store.OperationKey, intent model.Extension, rev model.Revision) error {
	var in labelConfigureIntent
	if err := readHeldIntent(intent, &in); err != nil {
		return err
	}
	if !in.Create {
		stored, err := tx.GetLabel(ctx, in.Label.Ref)
		if err := exactRef(err, stored.Ref == in.Label.Ref); err != nil {
			return err
		}
	}
	in.Label.Revision = rev
	return tx.PutLabel(ctx, in.Label, in.Expected)
}

// validateHeldLabelConfigureEdit admits the edit of a held label configure:
// the label's name, description and color may change; which label it is,
// whether it creates it and the revision it expects are the claim's.
func validateHeldLabelConfigureEdit(_ context.Context, _ store.Reader, _ store.OperationKey, stored, edited model.Extension) error {
	var in, old labelConfigureIntent
	if err := readHeldIntent(edited, &in); err != nil {
		return err
	}
	if err := json.Unmarshal(stored.Data, &old); err != nil {
		return err
	}
	if in.Label.Ref != old.Label.Ref {
		return model.NewError(model.Validation, "args: label must be the operation's target")
	}
	probe := in
	probe.Label.Name, probe.Label.Description, probe.Label.Color = old.Label.Name, old.Label.Description, old.Label.Color
	if !reflect.DeepEqual(probe, old) {
		return model.NewError(model.Validation, "args: only the name, description and color can change")
	}
	if err := in.Label.Validate(); err != nil {
		return model.NewError(model.Validation, "args: "+err.Error())
	}
	return nil
}

// --- label delete

type labelDeleteIntent struct {
	Label    model.LabelRef `json:"label"`
	Expected model.Revision `json:"expected_revision"`
}

func holdLabelDelete(ctx context.Context, c *engine.Client, req model.DeleteLabel) (model.Acceptance, error) {
	intent, err := storedIntent(intentLabelDelete, labelDeleteIntent{Label: req.Label, Expected: req.ExpectedRevision})
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Label.Ref()
	return operations.Submit(ctx, c, operations.Submission{
		Kind: model.ActionLabelConfigure, Target: res, Owner: res, Command: req.Command,
		Fingerprint: deleteLabelFingerprint(req), Intent: intent, Local: true, Result: &res,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			stored, err := tx.GetLabel(ctx, req.Label)
			if err := exactRef(err, stored.Ref == req.Label); err != nil {
				return err
			}
			if stored.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "label revision changed")
			}
			return nil
		},
	})
}

func applyHeldLabelDelete(ctx context.Context, _ *engine.Engine, tx store.Tx, _ model.Operation, _ store.OperationKey, intent model.Extension, _ model.Revision) error {
	var in labelDeleteIntent
	if err := readHeldIntent(intent, &in); err != nil {
		return err
	}
	stored, err := tx.GetLabel(ctx, in.Label)
	if err := exactRef(err, stored.Ref == in.Label); err != nil {
		return err
	}
	return tx.DeleteLabel(ctx, in.Label, in.Expected)
}

// validateHeldLabelDeleteEdit admits an edit of a held label delete only as
// the same delete of the same label: it has no argument to change.
func validateHeldLabelDeleteEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	var in, old labelDeleteIntent
	if err := readHeldIntent(edited, &in); err != nil {
		return err
	}
	if err := json.Unmarshal(stored.Data, &old); err != nil {
		return err
	}
	if in.Label.Ref() != key.Target {
		return model.NewError(model.Validation, "args: label must be the operation's target")
	}
	if in != old {
		return model.NewError(model.Validation, "args: a label delete has nothing to change")
	}
	return nil
}

// --- conversation transition

type transitionIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	State        string                `json:"state"`
	Correlation  model.CorrelationID   `json:"correlation,omitempty"`
}

// transition is one conversation status change, ready to write: the
// conversation, the status it moves to, the expected revision (empty skips
// the compare), the revision it moves to and its event ID.
type transition struct {
	conv        model.ConversationRef
	state       string
	expected    model.Revision
	correlation model.CorrelationID
	rev         model.Revision
	event       model.ID
}

// checkTransition loads the conversation of t in r and checks what writing
// it needs. It reports whether the status changes.
func checkTransition(ctx context.Context, r store.Reader, t transition) (cur model.Conversation, change bool, err error) {
	if err = loadConversation(ctx, r, t.conv, &cur); err != nil {
		return cur, false, err
	}
	if t.expected != "" && cur.Revision != t.expected {
		return cur, false, model.NewError(model.Conflict, "conversation revision changed")
	}
	if cur.Status == t.state {
		return cur, false, nil
	}
	if cur.Status == model.ConversationClosed {
		return cur, false, model.NewError(model.Conflict, "a closed conversation is terminal")
	}
	return cur, true, nil
}

// writeTransition writes t in tx as actor, under op: the conversation at its
// new status and revision and its conversation.changed event. Moving to the
// current status writes nothing.
func writeTransition(ctx context.Context, tx store.Tx, e *engine.Engine, actor model.ActorID, op model.OperationRef, t transition) error {
	cur, change, err := checkTransition(ctx, tx, t)
	if err != nil || !change {
		return err
	}
	next := cur.Clone()
	next.Status = t.state
	next.Revision = t.rev
	if err := tx.PutConversation(ctx, next, cur.Revision); err != nil {
		return err
	}
	ev, err := commandEventAs(e, actor, t.event, model.EventConversationChanged, model.SchemaConversationChanged,
		t.conv.Ref(), next.Revision, model.Audience{Visibility: model.Internal}, t.correlation,
		commandConversationChanged{
			Conversation: next.Ref, Changes: []string{model.ConversationFieldStatus}, Trigger: model.TriggerCommand,
			Status: &statusChange{From: cur.Status, To: next.Status}, PreviousRevision: cur.Revision, Operation: op,
		})
	if err != nil {
		return err
	}
	return tx.AppendEvents(ctx, []model.Event{ev})
}

func holdTransition(ctx context.Context, c *engine.Client, req model.Transition) (model.Acceptance, error) {
	intent, err := storedIntent(intentConversationTransition, transitionIntent{
		Conversation: req.Conversation, State: req.State, Correlation: req.Correlation,
	})
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Conversation.Ref()
	return operations.Submit(ctx, c, operations.Submission{
		Kind: model.ActionConversationTransition, Target: res, Owner: res, Command: req.Command,
		Fingerprint: transitionFingerprint(req), Intent: intent, Local: true, Result: &res,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			_, _, err := checkTransition(ctx, tx, transition{conv: req.Conversation, state: req.State, expected: req.ExpectedRevision})
			return err
		},
	})
}

func applyHeldTransition(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) error {
	var in transitionIntent
	if err := readHeldIntent(intent, &in); err != nil {
		return err
	}
	ids, err := allocate(ctx, e, 1)
	if err != nil {
		return err
	}
	return writeTransition(ctx, tx, e, key.Caller, op.Ref, transition{
		conv: in.Conversation, state: in.State, correlation: in.Correlation, rev: rev, event: ids[0],
	})
}

// validateHeldTransitionEdit admits the edit of a held transition: the
// status it moves to may change; the conversation and the correlation are
// the claim's.
func validateHeldTransitionEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	var in, old transitionIntent
	if err := readHeldIntent(edited, &in); err != nil {
		return err
	}
	if err := json.Unmarshal(stored.Data, &old); err != nil {
		return err
	}
	if in.Conversation.Ref() != key.Target {
		return model.NewError(model.Validation, "args: conversation must be the operation's target")
	}
	probe := in
	probe.State = old.State
	if probe != old {
		return model.NewError(model.Validation, "args: only the state can change")
	}
	if !model.IsConversationStatus(in.State) {
		return model.NewError(model.Validation, "args: state: must be a declared conversation status")
	}
	return nil
}
