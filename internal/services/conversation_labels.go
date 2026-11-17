package services

import (
	"cmp"
	"context"
	"slices"
	"strconv"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// AddLabels adds tenant labels to a conversation as one committed
// operation; see labelConversation.
func (s Conversations) AddLabels(ctx context.Context, req model.ConversationLabels) (model.Acceptance, error) {
	return labelConversation(ctx, s.c, req, true)
}

// RemoveLabels removes labels from a conversation as one committed
// operation; see labelConversation.
func (s Conversations) RemoveLabels(ctx context.Context, req model.ConversationLabels) (model.Acceptance, error) {
	return labelConversation(ctx, s.c, req, false)
}

// labelConversation adds (add) or removes the request's labels, authorized
// as model.ActionConversationLabel on the conversation at its revision; no
// label.read is asked. In the claiming unit every named label must be
// stored in the tenant, else not found and nothing is applied, and a set
// ExpectedRevision must be the conversation's. Labels already held (add)
// or not held (remove) change nothing; when none changes, the operation
// commits with no event and the revision unchanged. Otherwise the
// conversation is written once at a new revision with its labels sorted by
// ID, and one label_added or label_removed event is appended per changed
// label, in ID order. Growing past model.MaxConversationLabels is
// model.Conflict. A transient unit failure reruns the unit on fresh state,
// so two concurrent adds of one label make one change.
func labelConversation(ctx context.Context, c *engine.Client, req model.ConversationLabels, add bool) (model.Acceptance, error) {
	e := c.Engine()
	if req.Hold != nil {
		return holdLabels(ctx, c, req, add)
	}
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var conv model.Conversation
	if err := view(ctx, c, func(r store.Reader) error {
		return loadConversation(ctx, r, req.Conversation, &conv)
	}); err != nil {
		return model.Acceptance{}, err
	}
	// The conversation revision, two operation IDs, then one event ID per
	// named label: at most that many change.
	ids, err := allocate(ctx, e, 3+len(req.Labels))
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Conversation.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionConversationLabel, target: res, resource: res, owner: res,
		command: req.Command, fingerprint: conversationLabelsFingerprint(req, add), result: res,
		authorize: func(ctx context.Context) error {
			return c.Authorize(ctx, policy.Authorization{
				Action: model.ActionConversationLabel, Resource: res, Revision: conv.Revision,
			}, res)
		},
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			return writeLabelChange(ctx, tx, c.Engine(), c.Principal().Actor, op, labelChange{
				conv: req.Conversation, labels: req.Labels, add: add, expected: req.ExpectedRevision,
				correlation: req.Correlation, rev: model.Revision(ids[0]), events: ids[3:],
			})
		},
	}, ids[1], model.Revision(ids[2]))
}

// labelChange is one conversation label change, ready to write: the
// conversation, the labels and direction, the expected revision, the
// revision the conversation moves to and one event ID per label (at most
// that many change).
type labelChange struct {
	conv        model.ConversationRef
	labels      []model.LabelRef
	add         bool
	expected    model.Revision
	correlation model.CorrelationID
	rev         model.Revision
	events      []model.ID
}

// checkLabelChange loads the conversation of ch and checks, in tx, what
// writing it needs: the conversation's revision is the expected one and
// every named label is stored in the tenant (else not found).
func checkLabelChange(ctx context.Context, tx store.Tx, ch labelChange) (model.Conversation, error) {
	var cur model.Conversation
	if err := loadConversation(ctx, tx, ch.conv, &cur); err != nil {
		return cur, err
	}
	if ch.expected != "" && cur.Revision != ch.expected {
		return cur, model.NewError(model.Conflict, "conversation revision changed")
	}
	for _, l := range ch.labels {
		stored, err := tx.GetLabel(ctx, l)
		if err := exactRef(err, stored.Ref == l); err != nil {
			if model.IsCode(err, model.NotFound) {
				return cur, engine.ErrNotFound()
			}
			return cur, err
		}
	}
	return cur, nil
}

// writeLabelChange writes ch in tx as actor, under op: the conversation at
// its new revision and one label event per changed label. It writes
// nothing when no label changes.
func writeLabelChange(ctx context.Context, tx store.Tx, e *engine.Engine, actor model.ActorID, op model.OperationRef, ch labelChange) error {
	cur, err := checkLabelChange(ctx, tx, ch)
	if err != nil {
		return err
	}
	held, changed := applyLabels(cur.Labels, ch.labels, ch.add)
	if len(changed) == 0 {
		return nil
	}
	if len(held) > model.MaxConversationLabels {
		return model.NewError(model.Conflict,
			"labels: a conversation holds at most "+strconv.Itoa(model.MaxConversationLabels)+" labels")
	}
	next := cur.Clone()
	next.Labels = held
	next.Revision = ch.rev
	if err := tx.PutConversation(ctx, next, cur.Revision); err != nil {
		return err
	}
	kind, schema := model.EventConversationLabelAdded, model.SchemaConversationLabelAdded
	if !ch.add {
		kind, schema = model.EventConversationLabelRemoved, model.SchemaConversationLabelRemoved
	}
	evs := make([]model.Event, len(changed))
	for i, l := range changed {
		ev, err := commandEventAs(e, actor, ch.events[i], kind, schema, ch.conv.Ref(), next.Revision,
			model.Audience{Visibility: model.Internal}, ch.correlation,
			model.ConversationLabelPayload{
				Conversation: next.Ref, Label: l, Trigger: model.TriggerCommand,
				PreviousRevision: cur.Revision, Operation: &op,
			})
		if err != nil {
			return err
		}
		evs[i] = ev
	}
	return tx.AppendEvents(ctx, evs)
}

// applyLabels returns the labels held after adding (add) or removing
// labels, sorted by ID, and the labels that changed, in ID order.
func applyLabels(held, labels []model.LabelRef, add bool) (next, changed []model.LabelRef) {
	next = slices.Clone(held)
	for _, l := range labels {
		i := slices.Index(next, l)
		switch {
		case add && i < 0:
			next = append(next, l)
			changed = append(changed, l)
		case !add && i >= 0:
			next = slices.Delete(next, i, i+1)
			changed = append(changed, l)
		}
	}
	byID := func(a, b model.LabelRef) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(next, byID)
	slices.SortFunc(changed, byID)
	return next, changed
}

// conversationLabelsFingerprint returns the version 1 fingerprint of an
// AddLabels or RemoveLabels call: kind, target, direction, the label set
// (order-free) and expected revision.
func conversationLabelsFingerprint(req model.ConversationLabels, add bool) []byte {
	ids := make([]string, len(req.Labels))
	for i, l := range req.Labels {
		ids[i] = string(l.ID)
	}
	slices.Sort(ids)
	labels := operations.Field().Str(strconv.Itoa(len(ids)))
	for _, id := range ids {
		labels = labels.Str(id)
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionConversationLabel))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("add", operations.Field().Str(strconv.FormatBool(add)))
	f.Set("labels", labels)
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}
