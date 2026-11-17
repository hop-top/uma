package ingress

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/tasks"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// cancelAction names a remote cancel request in the operation fingerprint.
const cancelAction = "cancel_requested"

// CancelTask records a remote cancel request on a task the principal owns
// (Task.CancelRequested), as the operation of cmd under the service
// principal, authorized as task.act. The request commits with task.updated
// (cancel_requested, command-triggered) and changes nothing else: the host
// decides whether and when the task reaches canceled. It returns the task
// as stored afterwards. Retrying cmd replays; a new command on a task that
// already holds the request changes nothing and emits nothing. A terminal
// task is model.Conflict. cmd carries an operation ID and no expected
// revision.
func (s *Sink) CancelTask(ctx context.Context, principal model.ExternalRef, task model.TaskRef, cmd model.Command) (model.Task, error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.Task{}, err
	}
	defer done()
	if err := engine.FirstErr(s.checkPrincipal(principal), cmd.Validate(model.RevisionAbsent)); err != nil {
		return model.Task{}, err
	}
	var cur model.Task
	if err := s.view(ctx, func(r store.Reader) (err error) {
		_, _, cur, err = s.ownedTask(ctx, r, principal, task)
		return err
	}); err != nil {
		return model.Task{}, err
	}
	if err := s.authorize(ctx, policy.Authorization{Action: model.ActionTaskAct, Resource: task.Ref(), Revision: cur.Revision}); err != nil {
		return model.Task{}, err
	}
	opID, err := newID(ctx, s.e)
	if err != nil {
		return model.Task{}, err
	}
	opRev, err := operations.NewRevision(ctx, s.e)
	if err != nil {
		return model.Task{}, err
	}
	taskRev, err := operations.NewRevision(ctx, s.e)
	if err != nil {
		return model.Task{}, err
	}
	eventID, err := newID(ctx, s.e)
	if err != nil {
		return model.Task{}, err
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionTaskAct))
	f.Set("target", operations.Field().Ref(task.Ref()))
	f.Set("action", operations.Field().Str(cancelAction))
	if _, err := operations.CommitLocal(ctx, s.e.Bind(s.principal), operations.LocalCommand{
		Kind: model.ActionTaskAct, Target: task.Ref(), Command: cmd, Fingerprint: f.Sum(),
		Result: task.Ref(), ResultTask: &task,
		Apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			_, _, live, err := s.ownedTask(ctx, tx, principal, task)
			if err != nil {
				return err
			}
			ch, err := tasks.Cancel(live)
			if err != nil || len(ch.Changes) == 0 {
				return err
			}
			ch.Task.Revision = taskRev
			if err := tx.PutTask(ctx, ch.Task, live.Revision); err != nil {
				return err
			}
			ev, err := s.cancelEvent(eventID, ch, op, cmd.Correlation)
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, []model.Event{ev})
		},
	}, opID, opRev); err != nil {
		return model.Task{}, err
	}
	var out model.Task
	if err := s.view(ctx, func(r store.Reader) (err error) {
		_, _, out, err = s.ownedTask(ctx, r, principal, task)
		return err
	}); err != nil {
		return model.Task{}, err
	}
	return out.Clone(), nil
}

// cancelEvent is the task.updated event of a cancel request, caused by the
// operation op.
func (s *Sink) cancelEvent(id model.ID, ch tasks.Change, op model.OperationRef, correlation model.CorrelationID) (model.Event, error) {
	data, err := json.Marshal(tasks.Payload(ch, model.TriggerCommand, &op, nil))
	if err != nil {
		return model.Event{}, model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := s.e.Clock().Now().UTC()
	res := ch.Task.Ref.Ref()
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventTaskUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		Actor: s.principal.Actor, OccurredAt: now, RecordedAt: now,
		Correlation: correlation, Audience: model.Audience{Visibility: model.Internal}, Revision: ch.Task.Revision,
		Payload: model.Extension{Name: model.SchemaTaskUpdated, Version: model.PayloadVersion1, Data: data},
	}, nil
}
