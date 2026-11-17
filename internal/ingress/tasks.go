package ingress

import (
	"hop.top/uma/internal/tasks"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Agent contexts and task binding. A per-agent-context inbox groups
// conversations by the context reference the adapter supplies, never by
// the sender: the engine derives no context from a sender or a request
// field. The sender who creates a conversation owns it (Conversation.Owner).
// An envelope's task binding either opens a task for the message, in the
// message's own unit, or continues an open task of the inbox.

// carriesTask reports whether envelopes of kind may carry a task binding:
// new messages.
func carriesTask(kind string) bool {
	return kind == model.EnvelopeMessage || kind == model.EnvelopeAutomatedMessage
}

// checkTaskBinding validates an envelope's task binding against the bound
// channel: a well-formed binding whose continued task reference belongs to
// it. Whether the inbox accepts a binding is the unit's concern.
func (s *Sink) checkTaskBinding(env model.Envelope) error {
	b := env.Task
	if b == nil {
		return nil
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Open {
		return nil
	}
	return s.ownRef("envelope.task.external", b.External)
}

// agentContextGroup is the candidate group of one agent context in inbox:
// every conversation claimed under the context reference.
func agentContextGroup(inbox model.InboxRef, ref model.ExternalRef) store.SelectionKey {
	return store.SelectionKey{
		Inbox: inbox, Mode: model.ModePerAgentContext,
		Key: model.RefKindContext + ":" + lengthPrefixed(ref.Namespace, ref.ParentID, ref.ID),
	}
}

// contextRef returns the envelope's agent context reference. At most one
// is allowed: ok reports whether there is one.
func (u *ingressUnit) contextRef() (ref model.ExternalRef, ok bool, err error) {
	for _, r := range u.env.References {
		if r.Kind != model.RefKindContext {
			continue
		}
		if ok {
			return model.ExternalRef{}, false, model.NewError(model.Validation, "envelope.references: at most one context reference")
		}
		ref, ok = r, true
	}
	return ref, ok, nil
}

// agentContextScope is the candidate group of a per-agent-context
// selection: the context reference alone. The conversation records it as
// its external reference and reply route, and the sender as its owner.
func (u *ingressUnit) agentContextScope(sc conversationScope, sender *model.Identity) (conversationScope, error) {
	if sender == nil {
		return sc, model.NewError(model.Validation, "per-agent-context selection requires a sender")
	}
	ref, ok, err := u.contextRef()
	switch {
	case err != nil:
		return sc, err
	case !ok:
		return sc, model.NewError(model.Validation, "envelope.references: per-agent-context selection requires a "+model.RefKindContext+" reference")
	}
	owner := sender.Ref
	sc.group = agentContextGroup(u.inbox.Ref, ref)
	sc.external, sc.route, sc.owner = []model.ExternalRef{ref}, ref, &owner
	return sc, nil
}

// requireAgentContextMode rejects a task binding outside a
// per-agent-context inbox.
func (u *ingressUnit) requireAgentContextMode() error {
	if u.env.Task != nil && u.inbox.Selection.Mode != model.ModePerAgentContext {
		return model.NewError(model.Validation, "envelope.task: accepted only by a per-agent-context inbox")
	}
	return nil
}

// continuedTask resolves a continuation to its task: one of the inbox,
// not terminal. An unknown task is model.NotFound, a terminal one
// model.Conflict. It returns nil for any other envelope.
func (u *ingressUnit) continuedTask() (*model.Task, error) {
	b := u.env.Task
	if b == nil || b.Open {
		return nil, nil
	}
	t, err := u.tx.TaskByReference(u.ctx, u.inbox.Ref, b.External)
	if model.IsCode(err, model.NotFound) {
		return nil, model.NewError(model.NotFound, "envelope.task: no such task")
	}
	switch {
	case err != nil:
		return nil, err
	case model.IsTerminalTaskState(t.State):
		return nil, model.NewError(model.Conflict, "envelope.task: task is "+t.State)
	}
	return &t, nil
}

// targetConversation selects the conversation of a new message: the
// continued task's, else the one the inbox's selection picks. A context
// reference accompanying a continuation must be the task's own.
func (u *ingressUnit) targetConversation(sender *model.Identity, cont *model.Task) (model.Conversation, bool, error) {
	if cont == nil {
		return u.selectConversation(sender)
	}
	conv, err := u.tx.GetConversation(u.ctx, cont.Ref.Conversation)
	if err != nil {
		return model.Conversation{}, false, err
	}
	if given, ok, err := u.contextRef(); err != nil {
		return model.Conversation{}, false, err
	} else if ok {
		own, err := tasks.Context(conv)
		if err != nil {
			return model.Conversation{}, false, err
		}
		if given != own {
			return model.Conversation{}, false, model.NewError(model.Conflict, "envelope.references: context is not the task's context")
		}
	}
	if conv.Status == model.ConversationClosed {
		return model.Conversation{}, false, model.NewError(model.Conflict, "envelope.task: the task's conversation is closed")
	}
	conv, err = u.update(conv, sender, false, true)
	return conv, false, err
}

// bindTask binds msg, a new message of conv, to its task: the continued
// one, or a task opened for it, written in this unit. It returns nil for
// a message bound to none.
func (u *ingressUnit) bindTask(conv model.Conversation, msg *model.Message, cont *model.Task) (*taskBinding, error) {
	b := u.env.Task
	switch {
	case b == nil:
		return nil, nil
	case cont != nil:
		ref := cont.Ref
		msg.Task = &ref
		return &taskBinding{ref: ref}, nil
	}
	id, err := u.newID()
	if err != nil {
		return nil, err
	}
	rev, err := u.newRevision()
	if err != nil {
		return nil, err
	}
	ch, err := tasks.Open(conv, *msg, model.TaskOpen{Message: msg.Ref}, id, u.now)
	if err != nil {
		return nil, err
	}
	ch.Task.Revision = rev
	if err := u.tx.PutTask(u.ctx, ch.Task, ""); err != nil {
		return nil, err
	}
	ref := ch.Task.Ref
	msg.Task = &ref
	return &taskBinding{ref: ref, opened: &ch}, nil
}

// taskBinding is the task a message is bound to; opened is the opening
// write, set when the message opened it.
type taskBinding struct {
	ref    model.TaskRef
	opened *tasks.Change
}

// taskOpened queues task.updated (created, provider-triggered) for a task
// the message opened.
func (u *ingressUnit) taskOpened(b *taskBinding, msg model.MessageRef) error {
	if b == nil || b.opened == nil {
		return nil
	}
	ch := *b.opened
	return u.eventAs(model.Audience{Visibility: model.Internal}, model.EventTaskUpdated, model.SchemaTaskUpdated,
		b.ref.Ref(), ch.Task.Revision, tasks.Payload(ch, model.TriggerProvider, nil, &msg))
}

// receiptRefs are the references a per-agent-context message adds to its
// receipt.
type receiptRefs struct {
	conversation model.ConversationRef
	context      model.ExternalRef
	message      model.MessageRef
	task         *model.TaskRef
}

// correlate records what a new message of a per-agent-context inbox
// produced, for the receipt CompleteIngress persists with the unit: the
// conversation, its context, the message and the task it opened or
// continued.
func (u *ingressUnit) correlate(conv model.Conversation, msg model.Message, b *taskBinding) {
	if u.inbox.Selection.Mode != model.ModePerAgentContext {
		return
	}
	ctxRef, err := tasks.Context(conv)
	if err != nil {
		return
	}
	r := &receiptRefs{conversation: conv.Ref, context: ctxRef, message: msg.Ref}
	if b != nil {
		t := b.ref
		r.task = &t
	}
	u.correlation = r
}

// fill sets the receipt's references. A nil r sets none.
func (r *receiptRefs) fill(rc *model.IngressReceipt) {
	if r == nil {
		return
	}
	conv, ctxRef, msg := r.conversation, r.context, r.message
	rc.Conversation, rc.Context, rc.Message = &conv, &ctxRef, &msg
	if r.task != nil {
		t := *r.task
		rc.Task = &t
	}
}
