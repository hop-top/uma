package a2a

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Limits of the task reads.
const (
	defaultPageSize = 50
	maxPageSize     = 100
	// maxHistory bounds the history of one task view, newest messages kept.
	maxHistory = 100
	// historyPages bounds the pages read to find a task's messages.
	historyPages = 20
)

// dispatch runs one method. A nil result with a nil error means the client
// went away.
func (c *call) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "SendMessage":
		return c.sendMessage(ctx, req.Params)
	case "GetTask":
		return c.getTask(ctx, req.Params)
	case "ListTasks":
		return c.listTasks(ctx, req.Params)
	case "CancelTask":
		return c.cancelTask(ctx, req.Params)
	case "SendStreamingMessage", "SubscribeToTask":
		return nil, unsupportedOperation("STREAMING_NOT_SUPPORTED")
	case "GetExtendedAgentCard":
		return nil, unsupportedOperation("EXTENDED_AGENT_CARD_NOT_SUPPORTED")
	case "CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig":
		return nil, a2aError(codePushUnsupported, "Push notifications are not supported", "PUSH_NOTIFICATION_NOT_SUPPORTED", nil)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found"}
}

// taskRef is the core reference of the task a wire task ID names.
func (c *call) taskRef(id string) model.ExternalRef {
	return model.TaskExternalRef(c.x.ch, model.ID(id))
}

// contextRef is the reference of the context a wire context ID names.
func (c *call) contextRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: c.x.ch, Namespace: NamespaceContext, Kind: model.RefKindContext, ID: id}
}

// taskError maps a failure of a read or write about one task. A task that
// is absent and one the caller does not own are both -32001.
func (c *call) taskError(err error, taskID string) *rpcError {
	switch model.CodeOf(err) {
	case model.NotFound, model.Unauthorized:
		return taskNotFound(taskID)
	case model.Validation:
		return invalidParams("id", "is not a task ID")
	}
	return internalError(c.x.f.domain, err)
}

type getTaskParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength"`
}

func (c *call) getTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p getTaskParams
	if rerr := decodeParams(raw, &p); rerr != nil {
		return nil, rerr
	}
	if p.ID == "" {
		return nil, invalidParams("id", "required")
	}
	if p.HistoryLength != nil && *p.HistoryLength < 0 {
		return nil, invalidParams("historyLength", "must not be negative")
	}
	t, err := c.s.tasks.Task(ctx, c.principal, c.taskRef(p.ID))
	if err != nil {
		return nil, c.taskError(err, p.ID)
	}
	v, rerr := c.view(ctx, t, historyLength(p.HistoryLength), true)
	if rerr != nil {
		return nil, rerr
	}
	return v, nil
}

// historyLength is the number of history messages a request asks for:
// none unless it sets a positive length.
func historyLength(n *int) int {
	if n == nil {
		return 0
	}
	return min(*n, maxHistory)
}

// view renders t with its status message and, when history is positive,
// that many of its newest messages.
func (c *call) view(ctx context.Context, t model.Task, history int, artifacts bool) (wireTask, *rpcError) {
	if history == 0 && t.StatusMessage == nil {
		return taskView(t, nil, nil, artifacts), nil
	}
	msgs, err := c.taskMessages(ctx, t)
	if err != nil {
		return wireTask{}, c.taskError(err, t.External.ID)
	}
	var status *model.Message
	if t.StatusMessage != nil {
		for i := range msgs {
			if msgs[i].Ref == *t.StatusMessage {
				status = &msgs[i]
			}
		}
	}
	if history > 0 && len(msgs) > history {
		msgs = msgs[len(msgs)-history:]
	}
	if history == 0 {
		msgs = nil
	}
	return taskView(t, status, msgs, artifacts), nil
}

// taskMessages reads the messages bound to t in recording order, newest
// maxHistory kept.
func (c *call) taskMessages(ctx context.Context, t model.Task) ([]model.Message, error) {
	var out []model.Message
	req := model.PageRequest{Limit: maxPageSize}
	for range historyPages {
		page, err := c.s.tasks.TaskMessages(ctx, c.principal, t.Ref, req)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Complete || page.Next == req.After {
			break
		}
		req.After = page.Next
	}
	if len(out) > maxHistory {
		out = out[len(out)-maxHistory:]
	}
	return out, nil
}

type listParams struct {
	ContextID            string  `json:"contextId"`
	Status               string  `json:"status"`
	PageSize             *int    `json:"pageSize"`
	PageToken            string  `json:"pageToken"`
	HistoryLength        *int    `json:"historyLength"`
	StatusTimestampAfter *string `json:"statusTimestampAfter"`
	IncludeArtifacts     *bool   `json:"includeArtifacts"`
}

func (c *call) listTasks(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p listParams
	if rerr := decodeParams(raw, &p); rerr != nil {
		return nil, rerr
	}
	state, ok := umaState(p.Status)
	if !ok {
		return nil, invalidParams("status", "is not a TaskState")
	}
	size := defaultPageSize
	if p.PageSize != nil {
		if *p.PageSize < 1 || *p.PageSize > maxPageSize {
			return nil, invalidParams("pageSize", "must be from 1 to "+strconv.Itoa(maxPageSize))
		}
		size = *p.PageSize
	}
	if p.HistoryLength != nil && *p.HistoryLength < 0 {
		return nil, invalidParams("historyLength", "must not be negative")
	}
	var after time.Time
	if p.StatusTimestampAfter != nil {
		t, err := time.Parse(time.RFC3339Nano, *p.StatusTimestampAfter)
		if err != nil {
			return nil, invalidParams("statusTimestampAfter", "must be an RFC 3339 timestamp")
		}
		after = t
	}
	q := adapter.TaskQuery{Page: model.PageRequest{Limit: size, After: model.Cursor(p.PageToken)}}
	if state != "" {
		q.States = []string{state}
	}
	if p.ContextID != "" {
		ref := c.contextRef(p.ContextID)
		q.Context = &ref
	}
	page, err := c.s.tasks.Tasks(ctx, c.principal, q)
	if err != nil {
		return nil, c.listError(err)
	}
	history := historyLength(p.HistoryLength)
	artifacts := p.IncludeArtifacts != nil && *p.IncludeArtifacts
	out := listResult{Tasks: []wireTask{}, PageSize: size}
	for _, t := range page.Items {
		if !after.IsZero() && t.StatusAt.Before(after) {
			continue
		}
		v, rerr := c.view(ctx, t, history, artifacts)
		if rerr != nil {
			return nil, rerr
		}
		out.Tasks = append(out.Tasks, v)
	}
	if !page.Complete {
		out.NextPageToken = string(page.Next)
	}
	// uma counts nothing beyond the page it reads: totalSize is this
	// page's size.
	out.TotalSize = len(out.Tasks)
	return out, nil
}

// listError maps a failure of Tasks: a page token that is not the engine's
// and a principal that must name a context are the caller's parameters.
func (c *call) listError(err error) *rpcError {
	switch model.CodeOf(err) {
	case model.CursorExpired, model.Validation:
		return invalidParams("pageToken", "is not a valid page token")
	case model.Unsupported:
		return invalidParams("contextId", "required: the agent has tasks in several inboxes")
	}
	return internalError(c.x.f.domain, err)
}

type cancelParams struct {
	ID string `json:"id"`
}

func (c *call) cancelTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p cancelParams
	if rerr := decodeParams(raw, &p); rerr != nil {
		return nil, rerr
	}
	if p.ID == "" {
		return nil, invalidParams("id", "required")
	}
	ref := c.taskRef(p.ID)
	t, err := c.s.tasks.Task(ctx, c.principal, ref)
	if err != nil {
		return nil, c.taskError(err, p.ID)
	}
	// Checked here, not left to the engine: a repeat of the cancel request
	// replays its first operation, which would hide a terminal state.
	if model.IsTerminalTaskState(t.State) {
		return nil, cancelRefused(p.ID, t.State)
	}
	cmd := model.Command{
		Call:      model.Call{Correlation: "a2a.cancel"},
		Operation: cancelOperation(c.principal, p.ID),
	}
	got, err := c.s.tasks.CancelTask(ctx, c.principal, t.Ref, cmd)
	switch {
	case model.IsCode(err, model.Conflict):
		return nil, cancelRefused(p.ID, t.State)
	case err != nil:
		return nil, c.taskError(err, p.ID)
	}
	v, rerr := c.view(ctx, got, 0, true)
	if rerr != nil {
		return nil, rerr
	}
	return v, nil
}

func cancelRefused(taskID, state string) *rpcError {
	return a2aError(codeTaskNotCancelable, "Task cannot be canceled", "TASK_NOT_CANCELABLE",
		map[string]string{"taskId": taskID, "state": wireStates[state]})
}

// cancelOperation is the operation ID of a remote agent's cancel request
// for a task: the same for every repeat, so a repeated cancel replays one
// operation and stays a no-op.
func cancelOperation(principal model.ExternalRef, taskID string) model.OperationID {
	return model.OperationID("a2a.cancel." + digest("cancel", principal.Namespace, principal.ID, taskID))
}
