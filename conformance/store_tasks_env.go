package conformance

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// taskConcurrencyCases race units writing tasks under one reference.
var taskConcurrencyCases = []durableCase{
	{"ConcurrentTaskReferenceOneWins", func(t *testing.T, e *env, _ func(*testing.T)) {
		conv := e.putAgentConversation(t, nil)
		ext := e.externalRef(model.RefKindTask, "race")
		const workers = 8
		tasks := make([]model.Task, workers)
		errs := make([]error, workers)
		for i := range tasks {
			tasks[i] = e.task(model.TaskRef{Conversation: conv, ID: e.id(t)}, model.TaskSubmitted, e.at(i), "r1")
			tasks[i].External = ext
		}
		meet := newRendezvous(workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				errs[i] = e.overlapTx(meet, func(tx store.Tx) error { return tx.PutTask(e.ctx, tasks[i], "") })
			})
		}
		wg.Wait()
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil && winner >= 0:
				t.Fatalf("units %d and %d both stored a task under one reference", winner, i)
			case err == nil:
				winner = i
			default:
				wantCode(t, err, model.Conflict, "unit ", i, " losing to unit ", winner)
			}
		}
		if winner < 0 {
			t.Fatal("no unit stored the task")
		}
		e.wantTaskByReference(t, e.inbox, ext, tasks[winner])
		wantRefs(t, "tasks", e.listTasks(t, model.Filter{}), tasks[winner].Ref)
	}},
}

// at is a time s seconds after the fixture's base time.
func (e *env) at(s int) time.Time { return time.Unix(1_700_000_000+int64(s), 0).UTC() }

// ownerIdentity is an identity reference in the env's inbox; the store
// needs no identity record behind a conversation owner.
func (e *env) ownerIdentity(t *testing.T) model.IdentityRef {
	return model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
}

// putInbox stores another inbox on the env's channel.
func (e *env) putInbox(t *testing.T) model.InboxRef {
	t.Helper()
	ref := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error {
		return tx.PutInbox(e.ctx, model.Inbox{Ref: ref, Channel: e.channel, Name: "agents", Revision: "r1"}, "")
	})
	return ref
}

// contextRef is the agent context reference of conversation conv.
func (e *env) contextRef(conv model.ConversationRef) model.ExternalRef {
	return model.ExternalRef{Channel: e.channel, Namespace: "conformance.context", Kind: model.RefKindContext, ID: "ctx-" + string(conv.ID)}
}

// agentConversation is a per-agent-context conversation owned by owner.
func (e *env) agentConversation(ref model.ConversationRef, owner *model.IdentityRef, rev model.Revision) model.Conversation {
	c := e.conversation(ref, rev)
	c.Mode, c.Owner = model.ModePerAgentContext, owner
	c.External, c.ReplyRoute = []model.ExternalRef{e.contextRef(ref)}, e.contextRef(ref)
	return c
}

func (e *env) putAgentConversation(t *testing.T, owner *model.IdentityRef) model.ConversationRef {
	t.Helper()
	return e.putAgentConversationIn(t, e.inbox, owner)
}

func (e *env) putAgentConversationIn(t *testing.T, inbox model.InboxRef, owner *model.IdentityRef) model.ConversationRef {
	t.Helper()
	ref := model.ConversationRef{Inbox: inbox, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, e.agentConversation(ref, owner, "r1"), "") })
	return ref
}

// task is a task in state since at, under its core reference.
func (e *env) task(ref model.TaskRef, state string, at time.Time, rev model.Revision) model.Task {
	return model.Task{
		Ref: ref, Context: e.contextRef(ref.Conversation), External: model.TaskExternalRef(e.channel, ref.ID),
		State: state, StatusAt: at, CreatedAt: e.at(0), Revision: rev,
	}
}

// fullTask is a task with every field set. Each call builds fresh memory.
func (e *env) fullTask(ref model.TaskRef, rev model.Revision) model.Task {
	tk := e.task(ref, model.TaskInputRequired, e.at(1).Add(500*time.Millisecond+7), rev)
	tk.StatusMessage = &model.MessageRef{Conversation: ref.Conversation, ID: ref.ID}
	tk.CancelRequested = true
	tk.Artifacts = []model.Artifact{
		{ID: "report", Name: "Report", Description: "Quarterly", Complete: true,
			Content: model.Content{Parts: []model.Part{{Kind: "text", Text: "done"}}}},
		{ID: "log", Content: model.Content{Parts: []model.Part{{Kind: "text", Text: "partial"}}}},
	}
	return tk
}

func (e *env) putTask(t *testing.T, conv model.ConversationRef, state string, at time.Time) model.TaskRef {
	t.Helper()
	ref := model.TaskRef{Conversation: conv, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, e.task(ref, state, at, "r1"), "") })
	return ref
}

func (e *env) wantTask(t *testing.T, want model.Task) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.GetTask(e.ctx, want.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "task "+string(want.Ref.ID), got, want)
		}
		return err
	})
}

func (e *env) wantTaskByReference(t *testing.T, inbox model.InboxRef, ref model.ExternalRef, want model.Task) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.TaskByReference(e.ctx, inbox, ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "task by reference "+string(want.Ref.ID), got, want)
		}
		return err
	})
}

func (e *env) tasksPage(t *testing.T, req model.ListRequest) model.Page[model.Task] {
	t.Helper()
	var p model.Page[model.Task]
	e.mustView(t, func(r store.Reader) error {
		var err error
		p, err = r.Tasks(e.ctx, req)
		return err
	})
	return p
}

// listTasks pages the tasks under f one at a time.
func (e *env) listTasks(t *testing.T, f model.Filter) []model.TaskRef {
	t.Helper()
	return pageAll(t, func(after model.Cursor) (model.Page[model.Task], error) {
		return e.tasksPage(t, model.ListRequest{Filter: f, Page: model.PageRequest{Limit: 1, After: after}}), nil
	}, func(v model.Task) model.TaskRef { return v.Ref })
}

// listMessages pages the messages under f one at a time.
func (e *env) listMessages(t *testing.T, f model.Filter) []model.MessageRef {
	t.Helper()
	return pageAll(t, func(after model.Cursor) (p model.Page[model.Message], err error) {
		err = e.s.View(e.ctx, tenantA, func(r store.Reader) error {
			p, err = r.Messages(e.ctx, model.ListRequest{Filter: f, Page: model.PageRequest{Limit: 1, After: after}})
			return err
		})
		return p, err
	}, func(v model.Message) model.MessageRef { return v.Ref })
}

// replies pages the replies to parent one at a time.
func (e *env) replies(t *testing.T, parent model.MessageRef) []model.MessageRef {
	t.Helper()
	return pageAll(t, func(after model.Cursor) (p model.Page[model.Message], err error) {
		err = e.s.View(e.ctx, tenantA, func(r store.Reader) error {
			p, err = r.Replies(e.ctx, parent, model.PageRequest{Limit: 1, After: after})
			return err
		})
		return p, err
	}, func(v model.Message) model.MessageRef { return v.Ref })
}

// pageAll follows a paged read to its end and returns each item's key.
func pageAll[V, K any](t *testing.T, read func(model.Cursor) (model.Page[V], error), key func(V) K) []K {
	t.Helper()
	var out []K
	var after model.Cursor
	for range 100 {
		p, err := read(after)
		if err != nil {
			t.Fatalf("page after %q: %v", after, err)
		}
		for _, v := range p.Items {
			out = append(out, key(v))
		}
		if p.Complete {
			return out
		}
		after = p.Next
	}
	t.Fatal("paging did not complete")
	return nil
}

func taskRefs(tasks []model.Task) []model.TaskRef {
	out := make([]model.TaskRef, len(tasks))
	for i, v := range tasks {
		out[i] = v.Ref
	}
	return out
}

// wantRefs requires got to be exactly want, in order.
func wantRefs[T comparable](t *testing.T, what string, got []T, want ...T) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
	}
}

// linked is a message in conv bound to task and answering parent, either
// may be nil.
func (e *env) linked(t *testing.T, conv model.ConversationRef, task *model.TaskRef, parent *model.MessageRef) model.Message {
	m := e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "r1")
	m.Task, m.InReplyTo = task, parent
	return m
}

func (e *env) putLinked(t *testing.T, conv model.ConversationRef, task *model.TaskRef, parent *model.MessageRef) model.MessageRef {
	t.Helper()
	m := e.linked(t, conv, task, parent)
	e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, m, "") })
	return m.Ref
}

// mirrored is tenant B's copy of a tenant A task fixture.
type mirrored struct {
	owner    model.IdentityRef
	task     model.TaskRef
	message  model.MessageRef
	external model.ExternalRef
	// only is the ID of a task tenant B holds and tenant A does not.
	only model.ID
}

// mirrorTenant writes, in tenant B, the env's channel and inbox and a
// conversation owned by owner holding task tk, message msg bound to it and
// reply answering msg, every record under tenant A's IDs.
func (e *env) mirrorTenant(t *testing.T, owner model.IdentityRef, tk model.TaskRef, msg, reply model.MessageRef) mirrored {
	t.Helper()
	ch := model.ChannelRef{Tenant: tenantB, ID: e.channel.ID}
	in := model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}
	conv := model.ConversationRef{Inbox: in, ID: tk.Conversation.ID}
	m := mirrored{
		owner:    model.IdentityRef{Inbox: in, ID: owner.ID},
		task:     model.TaskRef{Conversation: conv, ID: tk.ID},
		message:  model.MessageRef{Conversation: conv, ID: msg.ID},
		external: model.TaskExternalRef(ch, tk.ID),
		only:     e.id(t),
	}
	err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
		c := e.agentConversation(conv, &m.owner, "r1")
		c.External = []model.ExternalRef{{Channel: ch, Namespace: "conformance.context", Kind: model.RefKindContext, ID: "ctx-" + string(conv.ID)}}
		c.ReplyRoute = c.External[0]
		task := e.task(m.task, model.TaskWorking, e.at(9), "r1")
		task.Context, task.External = c.External[0], m.external
		bound := e.message(m.message, "r1")
		bound.Task = &m.task
		answer := e.message(model.MessageRef{Conversation: conv, ID: reply.ID}, "r1")
		answer.InReplyTo = &m.message
		only := e.task(model.TaskRef{Conversation: conv, ID: m.only}, model.TaskWorking, e.at(9), "r1")
		only.Context, only.External = c.External[0], model.TaskExternalRef(ch, m.only)
		return firstErr(
			tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""),
			tx.PutInbox(e.ctx, model.Inbox{Ref: in, Channel: ch, Name: "mirror", Revision: "r1"}, ""),
			tx.PutConversation(e.ctx, c, ""),
			tx.PutTask(e.ctx, task, ""),
			tx.PutTask(e.ctx, only, ""),
			tx.PutMessage(e.ctx, bound, ""),
			tx.PutMessage(e.ctx, answer, ""),
		)
	})
	if err != nil {
		t.Fatalf("tenant B fixture: %v", err)
	}
	return m
}
