package ingress

import (
	"context"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// The task sink: the reads an agent protocol adapter answers and its cancel
// request. Every method runs as the sink's service principal, which the
// host authorizer decides for (task.read, conversation.read, message.read;
// task.act for a cancel). Beyond that, a remote principal reaches only the
// contexts and tasks it owns: conversations of per-agent-context inboxes of
// the bound channel whose Owner is the principal's identity there. What is
// absent and what the principal does not own are the same model.NotFound.
//
// A denial of the one resource a call is about (a context, a task, the
// message asked about, the task of a history) is model.Unauthorized. In a
// page, an item the service principal may not read is skipped without
// trace, as the Tasks and Messages services skip it.

var _ adapter.TaskSink = (*Sink)(nil)

// Page sizes of the sink's reads: a zero limit takes the default, a larger
// one is reduced to the maximum.
const (
	defaultTaskPage = 50
	maxTaskPage     = 100
)

func pageLimit(n int) int {
	switch {
	case n <= 0:
		return defaultTaskPage
	case n > maxTaskPage:
		return maxTaskPage
	}
	return n
}

// agentScope is one per-agent-context inbox of the bound channel in which
// the principal holds an identity.
type agentScope struct {
	inbox    model.Inbox
	identity model.Identity
}

// owns reports whether the scope's identity owns conv.
func (a agentScope) owns(conv model.Conversation) bool {
	return conv.Mode == model.ModePerAgentContext && conv.Ref.Inbox == a.inbox.Ref && conv.Owner != nil && *conv.Owner == a.identity.Ref
}

// checkPrincipal requires the principal to be a reference of the bound
// channel in a namespace the adapter declares.
func (s *Sink) checkPrincipal(p model.ExternalRef) error {
	if err := s.ownRef("principal", p); err != nil {
		return err
	}
	if !slices.Contains(s.desc.IdentityNamespaces, p.Namespace) {
		return model.NewError(model.Validation, "principal: namespace not declared by the adapter")
	}
	return nil
}

// scopes returns the per-agent-context inboxes of the bound channel where
// the principal has an identity.
func (s *Sink) scopes(ctx context.Context, r store.Reader, p model.ExternalRef) ([]agentScope, error) {
	var out []agentScope
	req := model.ListRequest{Page: model.PageRequest{Limit: maxTaskPage}}
	for {
		page, err := r.Inboxes(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, in := range page.Items {
			if in.Channel != s.channel.Ref || in.Selection.Mode != model.ModePerAgentContext {
				continue
			}
			id, err := r.Identity(ctx, model.IdentityKey{Inbox: in.Ref, Namespace: p.Namespace, SourceID: p.ID})
			switch {
			case model.IsCode(err, model.NotFound):
				continue
			case err != nil:
				return nil, err
			}
			out = append(out, agentScope{inbox: in, identity: id})
		}
		if page.Complete || page.Next == req.Page.After {
			return out, nil
		}
		req.Page.After = page.Next
	}
}

// ownedConversation returns the conversation ref when the principal owns
// it, with the scope that does.
func (s *Sink) ownedConversation(ctx context.Context, r store.Reader, p model.ExternalRef, ref model.ConversationRef) (agentScope, model.Conversation, error) {
	scopes, err := s.scopes(ctx, r, p)
	if err != nil {
		return agentScope{}, model.Conversation{}, err
	}
	for _, sc := range scopes {
		if sc.inbox.Ref != ref.Inbox {
			continue
		}
		conv, err := r.GetConversation(ctx, ref)
		if err != nil {
			return agentScope{}, model.Conversation{}, err
		}
		if conv.Ref == ref && sc.owns(conv) {
			return sc, conv, nil
		}
	}
	return agentScope{}, model.Conversation{}, engine.ErrNotFound()
}

// ownedTask returns the task with its scope and conversation when the
// principal owns its conversation.
func (s *Sink) ownedTask(ctx context.Context, r store.Reader, p model.ExternalRef, ref model.TaskRef) (agentScope, model.Conversation, model.Task, error) {
	if ref.Conversation.Inbox.Tenant != s.channel.Ref.Tenant {
		return agentScope{}, model.Conversation{}, model.Task{}, engine.ErrNotFound()
	}
	sc, conv, err := s.ownedConversation(ctx, r, p, ref.Conversation)
	if err != nil {
		return agentScope{}, model.Conversation{}, model.Task{}, err
	}
	t, err := r.GetTask(ctx, ref)
	if err != nil {
		return agentScope{}, model.Conversation{}, model.Task{}, err
	}
	if t.Ref != ref {
		return agentScope{}, model.Conversation{}, model.Task{}, engine.ErrNotFound()
	}
	return sc, conv, t, nil
}

// authorize asks the host authorizer about a, as the service principal.
func (s *Sink) authorize(ctx context.Context, a policy.Authorization) error {
	return s.e.Bind(s.principal).Authorize(ctx, a, a.Resource)
}

// readable reports whether the service principal may read under a. Only a
// denial is false; any other failure is returned.
func (s *Sink) readable(ctx context.Context, a policy.Authorization) (bool, error) {
	err := s.authorize(ctx, a)
	if model.IsCode(err, model.Unauthorized) {
		return false, nil
	}
	return err == nil, err
}

// view runs fn against the sink's tenant.
func (s *Sink) view(ctx context.Context, fn func(store.Reader) error) error {
	return s.e.Store().View(ctx, s.channel.Ref.Tenant, fn)
}

// Context returns the conversation of agent context ref when the principal
// owns it, authorized as conversation.read. When the context was
// re-opened under a new conversation after closing, the newest one the
// principal owns is returned.
func (s *Sink) Context(ctx context.Context, principal, ref model.ExternalRef) (model.Conversation, error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.Conversation{}, err
	}
	defer done()
	if err := s.checkPrincipal(principal); err != nil {
		return model.Conversation{}, err
	}
	if err := s.checkContext(ref); err != nil {
		return model.Conversation{}, err
	}
	var conv model.Conversation
	if err := s.view(ctx, func(r store.Reader) error {
		var err error
		conv, err = s.ownedContext(ctx, r, principal, ref)
		return err
	}); err != nil {
		return model.Conversation{}, err
	}
	if err := s.authorize(ctx, engine.ConversationRead(conv)); err != nil {
		return model.Conversation{}, err
	}
	// Workflow state is local desk state: it never reaches an adapter.
	return conv.Clone().WithoutWorkflow(), nil
}

func (s *Sink) checkContext(ref model.ExternalRef) error {
	if err := s.ownRef("context", ref); err != nil {
		return err
	}
	if ref.Kind != model.RefKindContext {
		return model.NewError(model.Validation, "context.kind: must be context")
	}
	return nil
}

// ownedContext finds the newest conversation of context ref the principal
// owns, in any of its scopes.
func (s *Sink) ownedContext(ctx context.Context, r store.Reader, p, ref model.ExternalRef) (model.Conversation, error) {
	scopes, err := s.scopes(ctx, r, p)
	if err != nil {
		return model.Conversation{}, err
	}
	var best *model.Conversation
	for _, sc := range scopes {
		cands, err := r.Candidates(ctx, agentContextGroup(sc.inbox.Ref, ref), candidateLimit)
		if err != nil {
			return model.Conversation{}, err
		}
		for _, c := range cands {
			if sc.owns(c) && (best == nil || later(c, *best)) {
				best = &c
			}
		}
	}
	if best == nil {
		return model.Conversation{}, engine.ErrNotFound()
	}
	return *best, nil
}

// Task returns the task whose core reference is ref when the principal
// owns its context, authorized as task.read.
func (s *Sink) Task(ctx context.Context, principal, ref model.ExternalRef) (model.Task, error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.Task{}, err
	}
	defer done()
	if err := s.checkPrincipal(principal); err != nil {
		return model.Task{}, err
	}
	if err := s.ownRef("task", ref); err != nil {
		return model.Task{}, err
	}
	if ref.Kind != model.RefKindTask {
		return model.Task{}, model.NewError(model.Validation, "task.kind: must be task")
	}
	var t model.Task
	if err := s.view(ctx, func(r store.Reader) error {
		scopes, err := s.scopes(ctx, r, principal)
		if err != nil {
			return err
		}
		for _, sc := range scopes {
			got, err := r.TaskByReference(ctx, sc.inbox.Ref, ref)
			if model.IsCode(err, model.NotFound) {
				continue
			} else if err != nil {
				return err
			}
			conv, err := r.GetConversation(ctx, got.Ref.Conversation)
			if err != nil {
				return err
			}
			if sc.owns(conv) {
				t = got
				return nil
			}
		}
		return engine.ErrNotFound()
	}); err != nil {
		return model.Task{}, err
	}
	if err := s.authorize(ctx, engine.TaskRead(t)); err != nil {
		return model.Task{}, err
	}
	return t.Clone(), nil
}

// Tasks pages the principal's tasks, most recent status first, each
// admitted under task.read. A principal with tasks in several
// per-agent-context inboxes of the channel can list one context at a time
// only; without a context filter that is model.Unsupported.
func (s *Sink) Tasks(ctx context.Context, principal model.ExternalRef, query adapter.TaskQuery) (model.Page[model.Task], error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.Page[model.Task]{}, err
	}
	defer done()
	if err := s.checkPrincipal(principal); err != nil {
		return model.Page[model.Task]{}, err
	}
	if err := query.Validate(); err != nil {
		return model.Page[model.Task]{}, err
	}
	if query.Context != nil {
		if err := s.checkContext(*query.Context); err != nil {
			return model.Page[model.Task]{}, err
		}
	}
	var page model.Page[model.Task]
	if err := s.view(ctx, func(r store.Reader) error {
		var err error
		page, err = s.listTasks(ctx, r, principal, query)
		return err
	}); err != nil {
		return model.Page[model.Task]{}, err
	}
	out := page
	out.Items = nil
	for _, t := range page.Items {
		ok, err := s.readable(ctx, engine.TaskRead(t))
		if err != nil {
			return model.Page[model.Task]{}, err
		}
		if ok {
			out.Items = append(out.Items, t.Clone())
		}
	}
	return out, nil
}

func (s *Sink) listTasks(ctx context.Context, r store.Reader, p model.ExternalRef, q adapter.TaskQuery) (model.Page[model.Task], error) {
	empty := model.Page[model.Task]{Complete: true}
	f := model.Filter{Statuses: slices.Clone(q.States)}
	if q.Context != nil {
		conv, err := s.ownedContext(ctx, r, p, *q.Context)
		if model.IsCode(err, model.NotFound) {
			return empty, nil
		} else if err != nil {
			return empty, err
		}
		sc, _, err := s.ownedConversation(ctx, r, p, conv.Ref)
		if err != nil {
			return empty, err
		}
		f.Inboxes, f.Identity, f.Conversation = []model.InboxRef{sc.inbox.Ref}, &sc.identity.Ref, &conv.Ref
	} else {
		scopes, err := s.scopes(ctx, r, p)
		switch {
		case err != nil:
			return empty, err
		case len(scopes) == 0:
			return empty, nil
		case len(scopes) > 1:
			return empty, engine.Unsupported("listing tasks across several per-agent-context inboxes")
		}
		f.Inboxes, f.Identity = []model.InboxRef{scopes[0].inbox.Ref}, &scopes[0].identity.Ref
	}
	q.Page.Limit = pageLimit(q.Page.Limit)
	return r.Tasks(ctx, model.ListRequest{Filter: f, Page: q.Page})
}

// TaskMessages pages the messages bound to a task the principal owns, in
// recording order. The task is authorized as task.read; each message is
// admitted under message.read.
func (s *Sink) TaskMessages(ctx context.Context, principal model.ExternalRef, task model.TaskRef, page model.PageRequest) (model.Page[model.Message], error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.Page[model.Message]{}, err
	}
	defer done()
	if err := s.checkPrincipal(principal); err != nil {
		return model.Page[model.Message]{}, err
	}
	var (
		t   model.Task
		got model.Page[model.Message]
	)
	if err := s.view(ctx, func(r store.Reader) error {
		sc, conv, owned, err := s.ownedTask(ctx, r, principal, task)
		if err != nil {
			return err
		}
		t = owned
		if err := s.authorize(ctx, engine.TaskRead(t)); err != nil {
			return err
		}
		page.Limit = pageLimit(page.Limit)
		got, err = r.Messages(ctx, model.ListRequest{
			Filter: model.Filter{Inboxes: []model.InboxRef{sc.inbox.Ref}, Conversation: &conv.Ref, Task: &task}, Page: page,
		})
		return err
	}); err != nil {
		return model.Page[model.Message]{}, err
	}
	out := got
	out.Items = nil
	for _, m := range got.Items {
		ok, err := s.readable(ctx, engine.MessageRead(m))
		if err != nil {
			return model.Page[model.Message]{}, err
		}
		if ok {
			out.Items = append(out.Items, m.Clone())
		}
	}
	return out, nil
}

// Responses reports what answers an accepted message of a context the
// principal owns. The message is authorized as message.read.
//
// The task is reported only when the message is bound to one and either
// opened it (it is the first message of the task's history) or the task's
// status changed after the message was recorded, so a continuation never
// answers with the task's earlier state; a status change at the very
// instant the message was recorded is not "after". Replies are every reply
// recorded for the message the service principal may read.
func (s *Sink) Responses(ctx context.Context, principal model.ExternalRef, message model.MessageRef) (adapter.Responses, error) {
	done, err := s.e.Begin()
	if err != nil {
		return adapter.Responses{}, err
	}
	defer done()
	if err := s.checkPrincipal(principal); err != nil {
		return adapter.Responses{}, err
	}
	var (
		msg     model.Message
		task    *model.Task
		replies []model.Message
	)
	if err := s.view(ctx, func(r store.Reader) error {
		if message.Conversation.Inbox.Tenant != s.channel.Ref.Tenant {
			return engine.ErrNotFound()
		}
		sc, conv, err := s.ownedConversation(ctx, r, principal, message.Conversation)
		if err != nil {
			return err
		}
		if msg, err = r.GetMessage(ctx, message); err != nil {
			return err
		}
		if msg.Ref != message {
			return engine.ErrNotFound()
		}
		if err := s.authorize(ctx, engine.MessageRead(msg)); err != nil {
			return err
		}
		if msg.Task != nil {
			if task, err = s.answeringTask(ctx, r, sc, conv, msg); err != nil {
				return err
			}
		}
		replies, err = s.repliesTo(ctx, r, msg.Ref)
		return err
	}); err != nil {
		return adapter.Responses{}, err
	}
	out := adapter.Responses{Replies: replies}
	if task != nil {
		t := task.Clone()
		out.Task = &t
	}
	return out, nil
}

// answeringTask returns the task of msg when it answers msg, nil when not.
func (s *Sink) answeringTask(ctx context.Context, r store.Reader, sc agentScope, conv model.Conversation, msg model.Message) (*model.Task, error) {
	t, err := r.GetTask(ctx, *msg.Task)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, engine.TaskRead(t)); err != nil {
		return nil, err
	}
	if t.StatusAt.After(msg.RecordedAt) {
		return &t, nil
	}
	first, err := r.Messages(ctx, model.ListRequest{
		Filter: model.Filter{Inboxes: []model.InboxRef{sc.inbox.Ref}, Conversation: &conv.Ref, Task: &t.Ref},
		Page:   model.PageRequest{Limit: 1},
	})
	if err != nil {
		return nil, err
	}
	if len(first.Items) == 1 && first.Items[0].Ref == msg.Ref {
		return &t, nil
	}
	return nil, nil
}

// repliesTo collects the readable replies to parent in recording order.
func (s *Sink) repliesTo(ctx context.Context, r store.Reader, parent model.MessageRef) ([]model.Message, error) {
	var out []model.Message
	req := model.PageRequest{Limit: maxTaskPage}
	for {
		page, err := r.Replies(ctx, parent, req)
		if err != nil {
			return nil, err
		}
		for _, m := range page.Items {
			if m.Origin == nil {
				continue // an inbound reply: Responses.Replies are uma-authored
			}
			ok, err := s.readable(ctx, engine.MessageRead(m))
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, m.Clone())
			}
		}
		if page.Complete || page.Next == req.After {
			return out, nil
		}
		req.After = page.Next
	}
}
