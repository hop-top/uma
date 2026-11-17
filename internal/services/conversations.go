package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Conversations is the Conversations service of one client.
type Conversations struct{ c *engine.Client }

// NewConversations returns the Conversations service bound to c.
func NewConversations(c *engine.Client) Conversations { return Conversations{c} }

// Get returns a conversation the principal may read. A conversation in
// another tenant or inbox than the reference names is not found.
func (s Conversations) Get(ctx context.Context, req model.Lookup[model.ConversationRef]) (model.Conversation, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Conversation{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Conversation{}, err
	}
	if req.Ref.Inbox.Tenant != c.Principal().Tenant {
		return model.Conversation{}, engine.ErrNotFound()
	}
	var conv model.Conversation
	if err := view(ctx, c, func(r store.Reader) error {
		return loadConversation(ctx, r, req.Ref, &conv)
	}); err != nil {
		return model.Conversation{}, err
	}
	if err := c.Authorize(ctx, conversationRead(conv), req.Ref.Ref()); err != nil {
		return model.Conversation{}, err
	}
	return shownConversation(ctx, c, conv.Clone())
}

func conversationRead(conv model.Conversation) policy.Authorization {
	return engine.ConversationRead(conv)
}

// List pages the conversations the principal may read, in recording order,
// optionally limited to Filter.Inboxes, Filter.Statuses, the labels they
// hold (Filter.Labels) and their workflow state (Filter.Assignee,
// Filter.Snoozed, Filter.SnoozeEnded, Filter.Linked, Filter.Unread), each evaluated by the
// store and, where the record allows, checked again here.
// Filter.Assignee.Self and an empty Filter.Unread.Actor are resolved to the
// principal's actor first; an unread actor other than the principal is
// model.Validation: no one lists another actor's read state. Under a
// workflow filter a conversation is shown only to a principal that may
// read its workflow state; any other is left out without trace.
func (s Conversations) List(ctx context.Context, req model.ListRequest) (model.Page[model.Conversation], error) {
	if u := req.Filter.Unread; u != nil && u.Actor != "" && u.Actor != s.c.Principal().Actor {
		return model.Page[model.Conversation]{}, model.NewError(model.Validation, "filter.unread.actor: must be the caller")
	}
	return list(ctx, s.c, listQuery[model.Conversation]{
		name: "conversations", read: store.Reader.Conversations,
		fields: filterFields{inboxes: true, statuses: true, labels: true, assignee: true, snoozed: true, snoozeEnded: true, linked: true, unread: true},
		within: func(f model.Filter, conv model.Conversation) bool {
			return inInboxes(f, conv.Ref.Inbox) && hasStatus(f, conv.Status) && hasLabels(f, conv.Labels) &&
				(f.Assignee == nil || f.Assignee.Matches(conv.Assignee))
		},
		admit: func(ctx context.Context, c *engine.Client, conv model.Conversation) error {
			return c.Authorize(ctx, conversationRead(conv), conv.Ref.Ref())
		},
		show:  showUnderWorkflowFilter,
		clone: model.Conversation.Clone,
	}, resolveSelf(s.c, req))
}

// resolveSelf returns req with Filter.Assignee.Self and an empty
// Filter.Unread.Actor replaced by the principal's actor, so the store, the
// recheck and the cursor see the actor they name.
func resolveSelf(c *engine.Client, req model.ListRequest) model.ListRequest {
	self := func(a *model.AssigneeFilter) bool { return a != nil && a.Self && a.Actor == "" && !a.None }
	if !self(req.Filter.Assignee) && (req.Filter.Unread == nil || req.Filter.Unread.Actor != "") {
		return req
	}
	req = req.Clone()
	if self(req.Filter.Assignee) {
		req.Filter.Assignee = &model.AssigneeFilter{Actor: c.Principal().Actor}
	}
	if req.Filter.Unread != nil && req.Filter.Unread.Actor == "" {
		req.Filter.Unread = &model.UnreadFilter{Actor: c.Principal().Actor}
	}
	return req
}

// Transition moves a conversation's local status at ExpectedRevision, as a
// committed operation, and records a conversation.changed event. It changes
// only local lifecycle: no provider ticket or thread is closed. A closed
// conversation is terminal; moving it is model.Conflict. Requesting the
// current status commits without change or event. Retrying with the same
// operation ID and payload returns the stored operation even after the
// conversation has moved on.
func (s Conversations) Transition(ctx context.Context, req model.Transition) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
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
	if err := view(ctx, c, func(r store.Reader) error {
		return loadConversation(ctx, r, req.Conversation, nil)
	}); err != nil {
		return model.Acceptance{}, err
	}
	if req.Hold != nil && e.Held().ConversationTransition {
		return holdTransition(ctx, c, req)
	}
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Conversation.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionConversationTransition, target: res, resource: res, owner: res,
		command: req.Command, fingerprint: transitionFingerprint(req), result: res,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			return writeTransition(ctx, tx, e, c.Principal().Actor, op, transition{
				conv: req.Conversation, state: req.State, expected: req.ExpectedRevision,
				correlation: req.Correlation, rev: model.Revision(ids[0]), event: ids[1],
			})
		},
	}, ids[2], model.Revision(ids[3]))
}

func transitionFingerprint(req model.Transition) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionConversationTransition))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("state", operations.Field().Str(req.State))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// loadConversation reads ref and requires the stored record to carry exactly
// that reference, whatever the backend matched on. A mismatch is not found.
func loadConversation(ctx context.Context, r store.Reader, ref model.ConversationRef, out *model.Conversation) error {
	conv, err := r.GetConversation(ctx, ref)
	if err != nil {
		return err
	}
	if conv.Ref != ref {
		return engine.ErrNotFound()
	}
	if out != nil {
		*out = conv
	}
	return nil
}
