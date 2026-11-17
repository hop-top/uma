package a2a

import (
	"context"
	"encoding/json"

	"hop.top/uma/model"
)

type sendParams struct {
	Message       wireMessage `json:"message"`
	Configuration struct {
		ReturnImmediately bool `json:"returnImmediately"`
		HistoryLength     *int `json:"historyLength"`
	} `json:"configuration"`
}

// target is where a message goes: the context reference to send under,
// and the task it continues, if any.
type target struct {
	// context is the context reference the envelope carries; zero when a
	// continuation lets the engine take the task's.
	context model.ExternalRef
	task    *model.Task
}

func (c *call) sendMessage(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p sendParams
	if rerr := decodeParams(raw, &p); rerr != nil {
		return nil, rerr
	}
	m := p.Message
	switch {
	case !validClientID(m.MessageID):
		return nil, invalidParams("message.messageId", "required: 1 to 128 printable characters")
	case m.Role != roleUser:
		return nil, invalidParams("message.role", "must be "+roleUser)
	case len(m.ReferenceTaskIDs) > maxReferenceTasks:
		return nil, invalidParams("message.referenceTaskIds", "too many references")
	case p.Configuration.HistoryLength != nil && *p.Configuration.HistoryLength < 0:
		return nil, invalidParams("configuration.historyLength", "must not be negative")
	}
	refs := make([]model.ExternalRef, 0, len(m.ReferenceTaskIDs)+1)
	for i, id := range m.ReferenceTaskIDs {
		ref := c.taskRef(id)
		if id == "" || ref.Validate() != nil {
			return nil, invalidParams("message.referenceTaskIds["+itoa(i)+"]", "is not a task ID")
		}
		refs = append(refs, ref)
	}
	// Ownership first: a context or task is reached only through the sink,
	// which answers for the principal that owns it. Nothing is staged or
	// accepted for one the principal cannot reach.
	tgt, rerr := c.resolveTarget(ctx, m)
	if rerr != nil {
		return nil, rerr
	}
	content, rerr := c.inboundContent(ctx, m.MessageID, c.x.cfg.Destination, m.Parts)
	if rerr != nil {
		return nil, rerr
	}
	env := model.Envelope{
		MessageID: inboundID(c.principal, m.MessageID), Destination: c.x.cfg.Destination,
		Kind: model.EnvelopeMessage, Sender: c.principal, SenderKind: model.ContactKindAgent,
		Assurance: "verified", Content: content, Audience: model.Audience{Visibility: model.Public},
		OccurredAt: c.x.clock.Now().UTC(), References: refs,
	}
	if !tgt.context.IsZero() {
		env.References = append(env.References, tgt.context)
	}
	switch {
	case tgt.task != nil:
		env.Task = &model.TaskBinding{External: tgt.task.External}
	case p.Configuration.ReturnImmediately:
		env.Task = &model.TaskBinding{Open: true}
	}
	receipt, err := c.s.Accept(ctx, model.Ingress{Envelope: env})
	if err != nil {
		return nil, c.acceptError(err, tgt, m)
	}
	if !receipt.Committed || receipt.Message == nil || receipt.Context == nil {
		// Claimed but not processed to the end: never answer with a new
		// context; the retry finds the first acceptance.
		return nil, umaError(c.x.f.domain, codeInternal, "Internal error", "UNAVAILABLE", nil)
	}
	history := historyLength(p.Configuration.HistoryLength)
	if receipt.Task != nil && p.Configuration.ReturnImmediately {
		return c.taskNow(ctx, receipt, history)
	}
	return c.await(ctx, receipt, m.MessageID, history)
}

// resolveTarget finds the context and task a message goes to, checking
// the principal owns them. A given context must exist and be the
// principal's (-32602), a given task must be the principal's (-32001),
// not terminal (-32004), and in the given context if both are given
// (-32602). Without either, a new opaque context is allocated.
func (c *call) resolveTarget(ctx context.Context, m wireMessage) (target, *rpcError) {
	var tgt target
	if m.TaskID != "" {
		t, err := c.s.tasks.Task(ctx, c.principal, c.taskRef(m.TaskID))
		if err != nil {
			return target{}, c.taskError(err, m.TaskID)
		}
		switch {
		case m.ContextID != "" && m.ContextID != t.Context.ID:
			return target{}, invalidParams("message.contextId", "is not the context of the task")
		case model.IsTerminalTaskState(t.State):
			return target{}, unsupportedOperation("TASK_TERMINAL")
		}
		tgt.task = &t
		return tgt, nil
	}
	if m.ContextID == "" {
		id, err := randomID()
		if err != nil {
			return target{}, internalError(c.x.f.domain, err)
		}
		tgt.context = c.contextRef(id)
		return tgt, nil
	}
	ref := c.contextRef(m.ContextID)
	if ref.Validate() != nil {
		return target{}, invalidParams("message.contextId", "is not a known context")
	}
	if _, err := c.s.tasks.Context(ctx, c.principal, ref); err != nil {
		switch model.CodeOf(err) {
		case model.NotFound, model.Unauthorized, model.Validation:
			return target{}, invalidParams("message.contextId", "is not a known context")
		}
		return target{}, internalError(c.x.f.domain, err)
	}
	tgt.context = ref
	return tgt, nil
}

// acceptError maps a refused ingress. For a continuation, an unknown task
// is -32001 and a task that ended meanwhile -32004; every other refusal is
// uma's, revealing nothing about what it refused.
func (c *call) acceptError(err error, tgt target, m wireMessage) *rpcError {
	if tgt.task != nil {
		switch model.CodeOf(err) {
		case model.NotFound, model.Unauthorized:
			return taskNotFound(m.TaskID)
		case model.Conflict:
			return unsupportedOperation("TASK_TERMINAL")
		}
	}
	if model.IsCode(err, model.Validation) {
		return invalidParams("message", "was refused")
	}
	return internalError(c.x.f.domain, err)
}

// taskNow answers with the task a message opened or continued, without
// waiting.
func (c *call) taskNow(ctx context.Context, r model.IngressReceipt, history int) (any, *rpcError) {
	t, err := c.s.tasks.Task(ctx, c.principal, model.TaskExternalRef(c.x.ch, r.Task.ID))
	if err != nil {
		return nil, c.taskError(err, string(r.Task.ID))
	}
	v, rerr := c.view(ctx, t, history, true)
	if rerr != nil {
		return nil, rerr
	}
	return sendResult{Task: &v}, nil
}
