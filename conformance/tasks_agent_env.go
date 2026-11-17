package conformance

import (
	"context"
	"encoding/json"
	"strconv"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Reads and host actions of the agent env.

// agentRead pages list under f to the end.
func agentRead[T any](env *agentEnv, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error), f model.Filter) []T {
	env.t.Helper()
	var out []T
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		req := model.ListRequest{Filter: f, Page: model.PageRequest{Limit: 100}}
		for {
			p, err := list(r, env.ctx, req)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	if err != nil {
		env.t.Fatalf("View: %v", err)
	}
	return out
}

func (env *agentEnv) inboxFilter() model.Filter {
	return model.Filter{Inboxes: []model.InboxRef{env.inbox}}
}

func (env *agentEnv) conversations() []model.Conversation {
	env.t.Helper()
	return agentRead(env, store.Reader.Conversations, env.inboxFilter())
}

func (env *agentEnv) allTasks() []model.Task {
	env.t.Helper()
	return agentRead(env, store.Reader.Tasks, env.inboxFilter())
}

func (env *agentEnv) allMessages() []model.Message {
	env.t.Helper()
	return agentRead(env, store.Reader.Messages, env.inboxFilter())
}

func (env *agentEnv) conversation(ref model.ConversationRef) model.Conversation {
	env.t.Helper()
	var c model.Conversation
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) { c, err = r.GetConversation(env.ctx, ref); return err })
	if err != nil {
		env.t.Fatalf("GetConversation: %v", err)
	}
	return c
}

func (env *agentEnv) message(ref model.MessageRef) model.Message {
	env.t.Helper()
	var m model.Message
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) { m, err = r.GetMessage(env.ctx, ref); return err })
	if err != nil {
		env.t.Fatalf("GetMessage: %v", err)
	}
	return m
}

func (env *agentEnv) identity(inbox model.InboxRef, p model.ExternalRef) (model.Identity, error) {
	var id model.Identity
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) {
		id, err = r.Identity(env.ctx, model.IdentityKey{Inbox: inbox, Namespace: p.Namespace, SourceID: p.ID})
		return err
	})
	return id, err
}

// events returns the committed events of the given kinds (all when none),
// in commit order.
func (env *agentEnv) events(kinds ...string) []model.Event {
	env.t.Helper()
	var out []model.Event
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		var after model.Cursor
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: kinds}, after, 100)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete || len(p.Items) == 0 && p.Next == after {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		env.t.Fatalf("Events: %v", err)
	}
	return out
}

// state is the full record state the cases compare to prove a refusal wrote
// nothing.
type agentState struct {
	conversations, messages, tasks, events int
}

func (env *agentEnv) state() agentState {
	env.t.Helper()
	return agentState{len(env.conversations()), len(env.allMessages()), len(env.allTasks()), len(env.events())}
}

func (env *agentEnv) wantState(want agentState, what string) {
	env.t.Helper()
	if got := env.state(); got != want {
		env.t.Fatalf("%s changed the records: %+v, want %+v", what, got, want)
	}
}

// Host actions: the host is the only executor of the work a remote agent
// asks for.

func (env *agentEnv) hostOpen(msg model.MessageRef, state string) model.Task {
	env.t.Helper()
	data, err := json.Marshal(model.TaskOpen{Message: msg, State: state})
	if err != nil {
		env.t.Fatal(err)
	}
	a, err := env.host.Tasks().Act(env.ctx, model.TaskAction{
		Command: env.cmd(""), Target: model.TaskRef{Conversation: msg.Conversation},
		Action: model.Extension{Name: model.SchemaTaskOpen, Version: 1, Data: data},
	})
	if err != nil {
		env.t.Fatalf("open task: %v", err)
	}
	if a.Operation.ResultTask == nil {
		env.t.Fatalf("open task operation = %+v", a.Operation)
	}
	return env.hostTask(*a.Operation.ResultTask)
}

func (env *agentEnv) hostTask(ref model.TaskRef) model.Task {
	env.t.Helper()
	t, err := env.host.Tasks().Get(env.ctx, model.Lookup[model.TaskRef]{Call: model.Call{Correlation: "agent-corr"}, Ref: ref})
	if err != nil {
		env.t.Fatalf("Tasks.Get: %v", err)
	}
	return t
}

// hostUpdateErr reports progress on task at its current revision.
func (env *agentEnv) hostUpdateErr(actor model.ActorID, task model.TaskRef, u model.TaskUpdate) error {
	env.t.Helper()
	data, err := json.Marshal(u)
	if err != nil {
		env.t.Fatal(err)
	}
	c, err := env.engine.For(model.Principal{Tenant: serviceTenant, Actor: actor})
	if err != nil {
		env.t.Fatal(err)
	}
	_, err = c.Tasks().Act(env.ctx, model.TaskAction{
		Command: env.cmd(env.hostTask(task).Revision), Target: task,
		Action: model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: data},
	})
	return err
}

func (env *agentEnv) hostUpdate(task model.TaskRef, u model.TaskUpdate) model.Task {
	env.t.Helper()
	if err := env.hostUpdateErr(agentHost, task, u); err != nil {
		env.t.Fatalf("update task: %v", err)
	}
	return env.hostTask(task)
}

// hostReply answers message m with text as the host.
func (env *agentEnv) hostReply(m model.MessageRef, text string) model.Message {
	env.t.Helper()
	a, err := env.host.Messages().Reply(env.ctx, model.Reply{
		Command: env.cmd(""), Conversation: m.Conversation, InReplyTo: &m, Content: textContent(text),
	})
	if err != nil {
		env.t.Fatalf("Reply: %v", err)
	}
	return env.message(*a.Operation.ResultMessage)
}

// consumer is a subscription of the host to events of kinds, from the
// beginning of the log.
type agentConsumer struct {
	env *agentEnv
	ref model.SubscriptionRef
}

func (env *agentEnv) consume(name string, kinds ...string) *agentConsumer {
	env.t.Helper()
	r, err := env.host.Events().Open(env.ctx, model.OpenSubscription{
		Command: env.cmd(""), Consumer: name, Filter: model.Filter{Kinds: kinds},
		Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		env.t.Fatalf("Events.Open: %v", err)
	}
	return &agentConsumer{env: env, ref: r.Subscription.Ref}
}

// next returns what arrived since the last call.
func (c *agentConsumer) next() []model.Event {
	c.env.t.Helper()
	var out []model.Event
	for range 100 {
		b, err := c.env.host.Events().Poll(c.env.ctx, model.Poll{Call: model.Call{Correlation: "agent-corr"}, Subscription: c.ref, Limit: 50})
		if err != nil {
			c.env.t.Fatalf("Events.Poll: %v", err)
		}
		out = append(out, b.Events...)
		if len(b.Events) == 0 {
			return out
		}
		if _, err := c.env.host.Events().Ack(c.env.ctx, model.Acknowledge{Command: c.env.cmd(""), Subscription: c.ref, Token: b.AckToken}); err != nil {
			c.env.t.Fatalf("Events.Ack: %v", err)
		}
	}
	c.env.t.Fatal("consumer never drained")
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
