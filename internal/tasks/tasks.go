// Package tasks holds the agent task rules Tasks.Act applies: opening a
// task for a message of a per-agent-context conversation, updating it along
// model.CanTransition, upserting its artifacts, recording a remote cancel
// request, and the task.updated payload they emit.
//
// The functions here are pure: callers load the records, allocate IDs and
// revisions, and write the result with its events in their own unit.
package tasks

import (
	"reflect"
	"slices"
	"time"

	"hop.top/uma/model"
)

// Change is a task write: the record to store under Previous, empty for a
// new task, and the task.updated changes its event reports. Changes is
// empty for an update that changes nothing, which commits without event.
type Change struct {
	Task     model.Task
	Previous model.Revision
	Changes  []string
	State    *model.StatusChange
}

// Context returns the agent context reference of conv: its reply route
// when that is of kind context, else its first external reference of that
// kind. Only a per-agent-context conversation holds tasks: any other is
// model.Validation, and so is one without a context reference.
func Context(conv model.Conversation) (model.ExternalRef, error) {
	if conv.Mode != model.ModePerAgentContext {
		return model.ExternalRef{}, model.NewError(model.Validation, "target: tasks exist only in per-agent-context conversations")
	}
	if conv.ReplyRoute.Kind == model.RefKindContext {
		return conv.ReplyRoute, nil
	}
	for _, r := range conv.External {
		if r.Kind == model.RefKindContext {
			return r, nil
		}
	}
	return model.ExternalRef{}, model.NewError(model.Validation, "target: conversation has no agent context")
}

// Open opens task id in conv for msg, as o asks, at now. The message must
// lie in conv and carry no task (model.Conflict otherwise: a message is
// bound once). The initial state is o.State, submitted when empty; it must
// be one model.CanTransition allows from no state. The caller sets the
// task's revision and binds the message (Message.Task).
func Open(conv model.Conversation, msg model.Message, o model.TaskOpen, id model.ID, now time.Time) (Change, error) {
	ctxRef, err := Context(conv)
	if err != nil {
		return Change{}, err
	}
	if !msg.Ref.In(conv.Ref) {
		return Change{}, model.NewError(model.Validation, "action.data.message: must belong to the target conversation")
	}
	if msg.Task != nil {
		return Change{}, model.NewError(model.Conflict, "action.data.message: already bound to a task")
	}
	state := o.State
	if state == "" {
		state = model.TaskSubmitted
	}
	if !model.CanTransition("", state) {
		return Change{}, model.NewError(model.Validation, "action.data.state: not a state a task can open in")
	}
	t := model.Task{
		Ref:     model.TaskRef{Conversation: conv.Ref, ID: id},
		Context: ctxRef, External: model.TaskExternalRef(ctxRef.Channel, id),
		State: state, StatusAt: now, CreatedAt: now,
	}
	return Change{Task: t, Changes: []string{model.TaskFieldCreated}, State: &model.StatusChange{To: state}}, nil
}

// Update applies u to cur at now. A terminal task never changes:
// model.Conflict, whatever u sets; so is a move model.CanTransition
// refuses. A state change or a status message sets StatusAt; reaching
// canceled clears CancelRequested. The caller records the status message
// and sets Task.StatusMessage and the revision. A Change without Changes
// means nothing changed.
func Update(cur model.Task, u model.TaskUpdate, now time.Time) (Change, error) {
	if model.IsTerminalTaskState(cur.State) {
		return Change{}, model.NewError(model.Conflict, "task is "+cur.State+": a terminal task never changes")
	}
	next := cur.Clone()
	ch := Change{Previous: cur.Revision}
	if u.State != "" && u.State != cur.State {
		if !model.CanTransition(cur.State, u.State) {
			return Change{}, model.NewError(model.Conflict, "task cannot move from "+cur.State+" to "+u.State)
		}
		next.State, next.StatusAt = u.State, now
		ch.Changes = append(ch.Changes, model.TaskFieldState)
		ch.State = &model.StatusChange{From: cur.State, To: u.State}
		if u.State == model.TaskCanceled && cur.CancelRequested {
			next.CancelRequested = false
			ch.Changes = append(ch.Changes, model.TaskFieldCancelRequested)
		}
	}
	if u.StatusMessage != nil {
		next.StatusAt = now
		ch.Changes = append(ch.Changes, model.TaskFieldStatusMessage)
	}
	if len(u.Artifacts) > 0 {
		next.Artifacts = Upsert(cur.Artifacts, u.Artifacts)
		if !reflect.DeepEqual(next.Artifacts, cur.Artifacts) {
			ch.Changes = append(ch.Changes, model.TaskFieldArtifacts)
		}
	}
	ch.Task = next
	return ch, nil
}

// Cancel records a remote cancel request on cur: CancelRequested is set
// and nothing else changes, for the host to answer by moving the task to
// canceled or on. A terminal task is model.Conflict; a task already
// requested is unchanged (a Change without Changes). The caller sets the
// revision.
func Cancel(cur model.Task) (Change, error) {
	if model.IsTerminalTaskState(cur.State) {
		return Change{}, model.NewError(model.Conflict, "task is "+cur.State+": a terminal task cannot be canceled")
	}
	next := cur.Clone()
	ch := Change{Task: next, Previous: cur.Revision}
	if cur.CancelRequested {
		return ch, nil
	}
	ch.Task.CancelRequested = true
	ch.Changes = []string{model.TaskFieldCancelRequested}
	return ch, nil
}

// Upsert returns arts with each update applied by artifact ID, in order:
// an update without Append replaces the artifact whole, or adds it; one
// with Append extends the artifact's parts, replaces a set Name or
// Description, and adds the artifact when new. Complete is the update's.
// New artifacts follow the existing ones in update order.
func Upsert(arts []model.Artifact, ups []model.ArtifactUpdate) []model.Artifact {
	out := make([]model.Artifact, 0, len(arts)+len(ups))
	for _, a := range arts {
		out = append(out, a.Clone())
	}
	for _, up := range ups {
		i := slices.IndexFunc(out, func(a model.Artifact) bool { return a.ID == up.ID })
		if i < 0 || !up.Append {
			a := model.Artifact{ID: up.ID, Name: up.Name, Description: up.Description, Content: up.Content.Clone(), Complete: up.Complete}
			if i < 0 {
				out = append(out, a)
			} else {
				out[i] = a
			}
			continue
		}
		a := &out[i]
		a.Content.Parts = append(a.Content.Parts, up.Content.Clone().Parts...)
		if up.Name != "" {
			a.Name = up.Name
		}
		if up.Description != "" {
			a.Description = up.Description
		}
		a.Complete = up.Complete
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Payload returns the task.updated payload of c, caused by trigger, under
// operation op, concerning message msg (the status message an update
// recorded, or the message a task was opened for).
func Payload(c Change, trigger string, op *model.OperationRef, msg *model.MessageRef) model.TaskUpdatedPayload {
	return model.TaskUpdatedPayload{
		Task: c.Task.Ref, Changes: slices.Clone(c.Changes), Trigger: trigger, State: c.State,
		PreviousRevision: c.Previous, Operation: op, Message: msg,
	}
}
