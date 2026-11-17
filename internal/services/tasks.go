package services

import (
	"context"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/tasks"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Tasks is the Tasks service of one client: authorized reads of agent
// tasks and the local Tasks.Act command. The task rules live in package
// tasks; this is their authorization and ledger boundary. Every caller is a
// host actor under the host authorizer; per-principal scoping of a remote
// agent's tasks belongs to the adapter task sink, not to this service.
type Tasks struct{ c *engine.Client }

// NewTasks returns the Tasks service bound to c.
func NewTasks(c *engine.Client) Tasks { return Tasks{c} }

// Get returns a task the principal may read (model.ActionTaskRead). A task
// outside the conversation, inbox or tenant the reference names is not
// found.
func (s Tasks) Get(ctx context.Context, req model.Lookup[model.TaskRef]) (model.Task, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Task{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Task{}, err
	}
	if req.Ref.Scope() != c.Principal().Tenant {
		return model.Task{}, engine.ErrNotFound()
	}
	var t model.Task
	if err := view(ctx, c, func(r store.Reader) error {
		return loadTask(ctx, r, req.Ref, &t)
	}); err != nil {
		return model.Task{}, err
	}
	if err := c.Authorize(ctx, taskRead(t), req.Ref.Ref()); err != nil {
		return model.Task{}, err
	}
	return t.Clone(), nil
}

func taskRead(t model.Task) policy.Authorization { return engine.TaskRead(t) }

// List pages the tasks the principal may read, most recent status first
// (the store's task order), optionally limited to Filter.Inboxes,
// Filter.Conversation and Filter.Statuses (task states). Any other filter
// field, Identity included, is model.Validation.
func (s Tasks) List(ctx context.Context, req model.ListRequest) (model.Page[model.Task], error) {
	return list(ctx, s.c, listQuery[model.Task]{
		name: "tasks", fields: filterFields{inboxes: true, conversation: true, statuses: true},
		require: func(f model.Filter) error {
			for _, st := range f.Statuses {
				if !model.IsTaskState(st) {
					return model.NewError(model.Validation, "filter.statuses: must be task states")
				}
			}
			return nil
		},
		read: store.Reader.Tasks,
		within: func(f model.Filter, t model.Task) bool {
			return inInboxes(f, t.Ref.Conversation.Inbox) && inConversation(f, t.Ref.Conversation) && hasStatus(f, t.State)
		},
		admit: func(ctx context.Context, c *engine.Client, t model.Task) error {
			return c.Authorize(ctx, taskRead(t), t.Ref.Ref())
		},
		clone: model.Task.Clone,
	}, req)
}

// Act applies a model.SchemaTaskOpen or model.SchemaTaskUpdate action as a
// committed operation whose ResultTask is the task; package tasks holds
// the rules. Retrying with the same operation ID and payload returns the
// stored operation even after the task moved on; a different payload is
// model.Conflict.
func (s Tasks) Act(ctx context.Context, req model.TaskAction) (model.Acceptance, error) {
	done, err := s.c.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	d, err := req.Decode()
	if err != nil {
		return model.Acceptance{}, err
	}
	if req.Target.Scope() != s.c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if d.Open != nil {
		return s.open(ctx, req, *d.Open)
	}
	return s.update(ctx, req, *d.Update)
}

// open opens a task for a message of the target conversation, authorized
// as model.ActionTaskAct on the conversation. It binds the message to the
// task and records task.updated (created) in the same unit.
func (s Tasks) open(ctx context.Context, req model.TaskAction, o model.TaskOpen) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	conv := req.Target.Conversation
	if err := view(ctx, c, func(r store.Reader) error {
		return loadConversation(ctx, r, conv, nil)
	}); err != nil {
		return model.Acceptance{}, err
	}
	ids, err := allocate(ctx, e, 6)
	if err != nil {
		return model.Acceptance{}, err
	}
	ref := model.TaskRef{Conversation: conv, ID: ids[0]}
	res := conv.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionTaskAct, target: operations.TaskActTarget(req), resource: res, owner: res,
		command: req.Command, fingerprint: operations.TaskActFingerprint(req),
		result: ref.Ref(), resultTask: &ref,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			var cur model.Conversation
			if err := loadConversation(ctx, tx, conv, &cur); err != nil {
				return err
			}
			m, err := tx.GetMessage(ctx, o.Message)
			if err == nil && m.Ref != o.Message {
				err = engine.ErrNotFound()
			}
			if err != nil {
				return err
			}
			ch, err := tasks.Open(cur, m, o, ref.ID, e.Clock().Now().UTC())
			if err != nil {
				return err
			}
			ch.Task.Revision = model.Revision(ids[1])
			if err := tx.PutTask(ctx, ch.Task, ""); err != nil {
				return err
			}
			prev := m.Revision
			m.Task, m.Revision = &ref, model.Revision(ids[2])
			if err := tx.PutMessage(ctx, m, prev); err != nil {
				return err
			}
			ev, err := commandEvent(c, ids[3], model.EventTaskUpdated, model.SchemaTaskUpdated,
				ref.Ref(), ch.Task.Revision, model.Audience{Visibility: model.Internal}, req.Correlation,
				tasks.Payload(ch, model.TriggerCommand, &op, &m.Ref))
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, []model.Event{ev})
		},
	}, ids[4], model.Revision(ids[5]))
}

// update applies a task update at ExpectedRevision, authorized as
// model.ActionTaskAct on the task (under the status message's audience
// when it carries one). Status content becomes exactly one uma-authored
// message bound to the task, origin model.MessageOriginTaskStatus,
// committed with the operation and never sent: the remote agent reads it
// through the task. An update that changes nothing commits without event.
func (s Tasks) update(ctx context.Context, req model.TaskAction, u model.TaskUpdate) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	if err := view(ctx, c, func(r store.Reader) error {
		return loadTask(ctx, r, req.Target, nil)
	}); err != nil {
		return model.Acceptance{}, err
	}
	ids, err := allocate(ctx, e, 7)
	if err != nil {
		return model.Acceptance{}, err
	}
	var audience model.Audience
	if u.StatusMessage != nil {
		audience = model.Audience{Visibility: model.Public}
		if u.Audience != nil {
			audience = *u.Audience
		}
	}
	target := req.Target
	res := target.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionTaskAct, target: operations.TaskActTarget(req), resource: res, owner: res, audience: audience,
		command: req.Command, fingerprint: operations.TaskActFingerprint(req),
		result: res, resultTask: &target,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			var cur model.Task
			if err := loadTask(ctx, tx, target, &cur); err != nil {
				return err
			}
			if cur.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "task revision changed")
			}
			now := e.Clock().Now().UTC()
			ch, err := tasks.Update(cur, u, now)
			if err != nil || len(ch.Changes) == 0 {
				return err
			}
			ch.Task.Revision = model.Revision(ids[0])
			var events []model.Event
			var status *model.MessageRef
			if u.StatusMessage != nil {
				ref := model.MessageRef{Conversation: target.Conversation, ID: ids[1]}
				ev, err := s.statusMessage(ctx, tx, target, u.StatusMessage.Clone(), audience, now,
					ref, model.Revision(ids[2]), ids[3], op, req.Correlation)
				if err != nil {
					return err
				}
				ch.Task.StatusMessage, status = &ref, &ref
				events = append(events, ev)
			}
			if err := tx.PutTask(ctx, ch.Task, cur.Revision); err != nil {
				return err
			}
			ev, err := commandEvent(c, ids[4], model.EventTaskUpdated, model.SchemaTaskUpdated,
				res, ch.Task.Revision, model.Audience{Visibility: model.Internal}, req.Correlation,
				tasks.Payload(ch, model.TriggerCommand, &op, status))
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, append(events, ev))
		},
	}, ids[5], model.Revision(ids[6]))
}

// statusMessage writes a task's status message ref, authored by the
// principal under operation op, and returns its message.created event.
// Source is the channel type of the task's inbox.
func (s Tasks) statusMessage(ctx context.Context, tx store.Tx, task model.TaskRef, content model.Content, audience model.Audience,
	now time.Time, ref model.MessageRef, rev model.Revision, event model.ID, op model.OperationRef, corr model.CorrelationID,
) (model.Event, error) {
	inbox, err := tx.GetInbox(ctx, task.Conversation.Inbox)
	if err != nil {
		return model.Event{}, err
	}
	channel, err := tx.GetChannel(ctx, inbox.Channel)
	if err != nil {
		return model.Event{}, err
	}
	m := model.Message{
		Ref: ref, Content: content, Audience: audience, Task: &task,
		OccurredAt: now, RecordedAt: now, Revision: rev, Source: channel.Type,
		Origin: &model.Origin{Actor: s.c.Principal().Actor, Operation: op},
	}
	if err := tx.PutMessage(ctx, m, ""); err != nil {
		return model.Event{}, err
	}
	return commandEvent(s.c, event, model.EventMessageCreated, model.SchemaMessageCreated, ref.Ref(), rev, audience, corr,
		commandMessageCreated{Message: ref, Trigger: model.TriggerCommand, Origin: model.MessageOriginTaskStatus, Operation: op, Source: channel.Type})
}

// loadTask reads ref and requires the stored record to carry exactly that
// reference. A mismatch is not found.
func loadTask(ctx context.Context, r store.Reader, ref model.TaskRef, out *model.Task) error {
	t, err := r.GetTask(ctx, ref)
	if err != nil {
		return err
	}
	if t.Ref != ref {
		return engine.ErrNotFound()
	}
	if out != nil {
		*out = t
	}
	return nil
}
