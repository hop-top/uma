package services

import (
	"context"
	"strconv"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Assign sets the conversation's assignee as one committed operation; see
// workflowCommand.
func (s Conversations) Assign(ctx context.Context, req model.ConversationAssignment) (model.Acceptance, error) {
	done, err := s.c.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Assignee == "" {
		return model.Acceptance{}, model.NewError(model.Validation, "assignee: required")
	}
	return assignConversation(ctx, s.c, req)
}

// Unassign clears the conversation's assignee as one committed operation;
// see workflowCommand.
func (s Conversations) Unassign(ctx context.Context, req model.ConversationAssignment) (model.Acceptance, error) {
	done, err := s.c.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Assignee != "" {
		return model.Acceptance{}, model.NewError(model.Validation, "assignee: must be empty to unassign")
	}
	return assignConversation(ctx, s.c, req)
}

// Snooze snoozes the conversation until req.Until as one committed
// operation; see workflowCommand.
func (s Conversations) Snooze(ctx context.Context, req model.ConversationSnooze) (model.Acceptance, error) {
	done, err := s.c.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Until == nil {
		return model.Acceptance{}, model.NewError(model.Validation, "until: required")
	}
	if !req.Until.After(s.c.Engine().Clock().Now()) {
		return model.Acceptance{}, model.NewError(model.Validation, "until: must be in the future")
	}
	return snoozeConversation(ctx, s.c, req)
}

// Unsnooze ends the conversation's snooze as one committed operation; see
// workflowCommand.
func (s Conversations) Unsnooze(ctx context.Context, req model.ConversationSnooze) (model.Acceptance, error) {
	done, err := s.c.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Until != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "until: must be unset to unsnooze")
	}
	return snoozeConversation(ctx, s.c, req)
}

// workflowChange is one workflow mutation of a conversation: next returns
// the changed record and the event payload, or ok false when the call
// changes nothing.
type workflowChange struct {
	kind, event, schema string
	command             model.Command
	conversation        model.ConversationRef
	fingerprint         []byte
	// eligible, when set, runs after the caller's authorization.
	eligible func(ctx context.Context, conv model.Conversation) error
	next     func(ctx context.Context, tx store.Tx, cur model.Conversation, op model.OperationRef) (next model.Conversation, payload any, ok bool, err error)
}

// workflowCommand runs a workflow change as a committed operation,
// authorized as its kind on the conversation at its revision. In the
// claiming unit a set ExpectedRevision must be the conversation's, else
// model.Conflict. A call that changes nothing commits with no event and the
// revision unchanged; otherwise the conversation is written once at a new
// revision and one event is appended, internal audience, references only.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts.
func workflowCommand(ctx context.Context, c *engine.Client, w workflowChange) (model.Acceptance, error) {
	e := c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if w.conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var conv model.Conversation
	if err := view(ctx, c, func(r store.Reader) error {
		return loadConversation(ctx, r, w.conversation, &conv)
	}); err != nil {
		return model.Acceptance{}, err
	}
	// The conversation revision, two operation IDs, one event ID.
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := w.conversation.Ref()
	return commit(ctx, c, localCommand{
		kind: w.kind, target: res, resource: res, owner: res,
		command: w.command, fingerprint: w.fingerprint, result: res,
		authorize: func(ctx context.Context) error {
			if err := c.Authorize(ctx, policy.Authorization{Action: w.kind, Resource: res, Revision: conv.Revision}, res); err != nil {
				return err
			}
			if w.eligible != nil {
				return w.eligible(ctx, conv)
			}
			return nil
		},
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			var cur model.Conversation
			if err := loadConversation(ctx, tx, w.conversation, &cur); err != nil {
				return err
			}
			if w.command.ExpectedRevision != "" && cur.Revision != w.command.ExpectedRevision {
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			next, payload, ok, err := w.next(ctx, tx, cur.Clone(), op)
			if err != nil || !ok {
				return err
			}
			next.Revision = model.Revision(ids[0])
			if err := tx.PutConversation(ctx, next, cur.Revision); err != nil {
				return err
			}
			ev, err := commandEvent(c, ids[3], w.event, w.schema, res, next.Revision,
				model.Audience{Visibility: model.Internal}, w.command.Correlation, payload)
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, []model.Event{ev})
		},
	}, ids[1], model.Revision(ids[2]))
}

// assignConversation sets (req.Assignee) or clears the assignee,
// authorized as model.ActionConversationAssign on the conversation. An
// assignee must be eligible: the authorizer is asked whether the assignee
// itself may perform model.ActionConversationAssign on the conversation,
// and a denial is model.Unauthorized. Assigning the current assignee, or
// unassigning an unassigned conversation, changes nothing.
func assignConversation(ctx context.Context, c *engine.Client, req model.ConversationAssignment) (model.Acceptance, error) {
	w := workflowChange{
		kind: model.ActionConversationAssign, event: model.EventConversationAssigned, schema: model.SchemaConversationAssigned,
		command: req.Command, conversation: req.Conversation, fingerprint: assignmentFingerprint(req),
	}
	if req.Assignee == "" {
		w.event, w.schema = model.EventConversationUnassigned, model.SchemaConversationUnassigned
	} else {
		w.eligible = func(ctx context.Context, conv model.Conversation) error {
			return assigneeEligible(ctx, c, req.Assignee, conv)
		}
	}
	w.next = func(_ context.Context, _ store.Tx, cur model.Conversation, op model.OperationRef) (model.Conversation, any, bool, error) {
		prev := cur.Assignee
		switch {
		case req.Assignee == "" && prev == nil:
			return cur, nil, false, nil
		case req.Assignee != "" && prev != nil && *prev == req.Assignee:
			return cur, nil, false, nil
		}
		var to *model.ActorID
		if req.Assignee != "" {
			a := req.Assignee
			to = &a
		}
		payload := model.ConversationAssignmentPayload{
			Conversation: cur.Ref, Assignee: to, Previous: prev, Trigger: model.TriggerCommand,
			PreviousRevision: cur.Revision, Operation: &op,
		}
		cur.Assignee = to
		return cur, payload, true, nil
	}
	return workflowCommand(ctx, c, w)
}

// assigneeEligible asks the authorizer whether assignee may itself
// perform model.ActionConversationAssign on conv: a host admits as
// assignees exactly the actors it lets work the conversation.
func assigneeEligible(ctx context.Context, c *engine.Client, assignee model.ActorID, conv model.Conversation) error {
	as, err := c.Engine().For(model.Principal{Tenant: c.Principal().Tenant, Actor: assignee})
	if err != nil {
		return err
	}
	res := conv.Ref.Ref()
	err = as.Authorize(ctx, policy.Authorization{Action: model.ActionConversationAssign, Resource: res, Revision: conv.Revision}, res)
	if model.IsCode(err, model.Unauthorized) {
		return model.NewError(model.Unauthorized, "assignee is not eligible for this conversation")
	}
	return err
}

// snoozeConversation sets (req.Until) or ends the snooze, authorized as
// model.ActionConversationSnooze on the conversation. A snooze records the
// engine clock's now as At and the caller as By, replacing any snooze, so
// only live inbound messages recorded after it, never imported ones, wake
// the conversation. Ending a snooze that is not in effect (none, expired
// or woken by an inbound message) changes nothing.
func snoozeConversation(ctx context.Context, c *engine.Client, req model.ConversationSnooze) (model.Acceptance, error) {
	w := workflowChange{
		kind: model.ActionConversationSnooze, event: model.EventConversationSnoozed, schema: model.SchemaConversationSnoozed,
		command: req.Command, conversation: req.Conversation, fingerprint: snoozeFingerprint(req),
	}
	if req.Until == nil {
		w.event, w.schema = model.EventConversationUnsnoozed, model.SchemaConversationUnsnoozed
	}
	w.next = func(ctx context.Context, tx store.Tx, cur model.Conversation, op model.OperationRef) (model.Conversation, any, bool, error) {
		now := c.Engine().Clock().Now().UTC()
		payload := model.ConversationSnoozePayload{
			Conversation: cur.Ref, Trigger: model.TriggerCommand, PreviousRevision: cur.Revision, Operation: &op,
		}
		if req.Until == nil {
			in, err := snoozeInEffect(ctx, tx, cur, now)
			if err != nil || !in {
				return cur, nil, false, err
			}
			cur.Snooze = nil
			return cur, payload, true, nil
		}
		until := req.Until.UTC()
		cur.Snooze = &model.Snooze{Until: until, At: now, By: c.Principal().Actor}
		payload.Until = &until
		return cur, payload, true, nil
	}
	return workflowCommand(ctx, c, w)
}

// snoozeInEffect reports whether conv's snooze hides it at now: its newest
// live inbound message that was not imported (newestLiveInbound) is read
// only when it holds a snooze.
func snoozeInEffect(ctx context.Context, r store.Reader, conv model.Conversation, now time.Time) (bool, error) {
	if conv.Snooze == nil {
		return false, nil
	}
	m, err := newestLiveInbound(ctx, r, conv.Ref, &conv.Snooze.At)
	if err != nil {
		return false, err
	}
	var newest *time.Time
	if m != nil {
		newest = &m.RecordedAt
	}
	return conv.Snooze.InEffect(now, newest), nil
}

// inboundPage is the page size newestLiveInbound reads with past the
// first message.
const inboundPage = 50

// newestLiveInbound returns conv's newest live inbound message
// (Reader.InboundMessages) that was not imported from the provider's
// history (Message.ImportedBy), or nil when it has none. Imported messages
// are recorded when the import ran, so they come first and are passed
// over; with after set the read stops at the first message recorded at or
// before it, and nil then means none was recorded after it. The first
// read takes one message, the common case without imports.
func newestLiveInbound(ctx context.Context, r store.Reader, conv model.ConversationRef, after *time.Time) (*model.Message, error) {
	page := model.PageRequest{Limit: 1}
	for {
		pg, err := r.InboundMessages(ctx, conv, page)
		if err != nil {
			return nil, err
		}
		for i := range pg.Items {
			m := &pg.Items[i]
			switch {
			case after != nil && !m.RecordedAt.After(*after):
				return nil, nil
			case m.ImportedBy == "":
				return m, nil
			}
		}
		if pg.Complete || pg.Next == "" || len(pg.Items) == 0 {
			return nil, nil
		}
		page = model.PageRequest{Limit: inboundPage, After: pg.Next}
	}
}

// assignmentFingerprint returns the version 1 fingerprint of an Assign or
// Unassign call: kind, target, assignee (empty for Unassign) and expected
// revision.
func assignmentFingerprint(req model.ConversationAssignment) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionConversationAssign))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("assignee", operations.Field().Str(string(req.Assignee)))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// snoozeFingerprint returns the version 1 fingerprint of a Snooze or
// Unsnooze call: kind, target, the until instant ("-" for Unsnooze) and
// expected revision.
func snoozeFingerprint(req model.ConversationSnooze) []byte {
	until := operations.Field().Str("-")
	if req.Until != nil {
		until = operations.Field().Str(strconv.FormatInt(req.Until.Unix(), 10)).Str(strconv.Itoa(req.Until.Nanosecond()))
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionConversationSnooze))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("until", until)
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}
