package conformance

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// newTaskEnv is a service env whose channel adapter also declares send, so
// replies in agent conversations are accepted.
func newTaskEnv(t *testing.T, factory StoreFactory) *serviceEnv {
	t.Helper()
	return newServiceEnvWith(t, factory, serviceAdapter{caps: []model.Capability{model.CapabilitySend}})
}

// agentConversation stores a per-agent-context conversation in the env's
// first inbox, routed on its agent context reference.
func (env *serviceEnv) agentConversation() model.Conversation {
	env.t.Helper()
	ref := model.ConversationRef{Inbox: env.inbox, ID: env.id()}
	ctx := model.ExternalRef{Channel: env.ch, Namespace: "com.example.conformance.context", Kind: model.RefKindContext, ID: "ctx-" + string(ref.ID)}
	c := model.Conversation{
		Ref: ref, Mode: model.ModePerAgentContext, Status: model.ConversationOpen,
		External: []model.ExternalRef{ctx}, ReplyRoute: ctx, Revision: "cv1",
	}
	env.put(func(tx store.Tx) error { return tx.PutConversation(env.ctx, c, "") })
	return c
}

func taskData(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// openReq is a SchemaTaskOpen request for msg in conv.
func (env *serviceEnv) openReq(conv model.ConversationRef, msg model.MessageRef, state string) model.TaskAction {
	return model.TaskAction{
		Command: env.cmd(""), Target: model.TaskRef{Conversation: conv},
		Action: model.Extension{Name: model.SchemaTaskOpen, Version: 1, Data: taskData(env.t, model.TaskOpen{Message: msg, State: state})},
	}
}

// updateReq is a SchemaTaskUpdate request for task at rev.
func (env *serviceEnv) updateReq(task model.TaskRef, rev model.Revision, u model.TaskUpdate) model.TaskAction {
	return model.TaskAction{
		Command: env.cmd(rev), Target: task,
		Action: model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: taskData(env.t, u)},
	}
}

// openTask opens a task for msg and returns it.
func (env *serviceEnv) openTask(conv model.ConversationRef, msg model.MessageRef, state string) model.Task {
	env.t.Helper()
	a, err := env.client.Tasks().Act(env.ctx, env.openReq(conv, msg, state))
	if err != nil {
		env.t.Fatalf("open task: %v", err)
	}
	if a.Operation.ResultTask == nil || a.Operation.State != model.Committed {
		env.t.Fatalf("open task operation = %+v", a.Operation)
	}
	return env.getTask(*a.Operation.ResultTask)
}

// updateTask applies u to task at its current revision and returns the
// task after it.
func (env *serviceEnv) updateTask(task model.Task, u model.TaskUpdate) model.Task {
	env.t.Helper()
	if _, err := env.client.Tasks().Act(env.ctx, env.updateReq(task.Ref, task.Revision, u)); err != nil {
		env.t.Fatalf("update task: %v", err)
	}
	return env.getTask(task.Ref)
}

func (env *serviceEnv) getTask(ref model.TaskRef) model.Task {
	env.t.Helper()
	t, err := env.client.Tasks().Get(env.ctx, model.Lookup[model.TaskRef]{Call: model.Call{Correlation: "corr"}, Ref: ref})
	if err != nil {
		env.t.Fatalf("Tasks.Get: %v", err)
	}
	return t
}

func (env *serviceEnv) getMessage(ref model.MessageRef) model.Message {
	env.t.Helper()
	var m model.Message
	if err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) { m, err = r.GetMessage(env.ctx, ref); return err }); err != nil {
		env.t.Fatalf("GetMessage: %v", err)
	}
	return m
}

// listTasks pages Tasks.List to completion and returns the task refs.
func (env *serviceEnv) listTasks(f model.Filter, limit int) []model.TaskRef {
	env.t.Helper()
	var out []model.TaskRef
	var after model.Cursor
	for range 1000 {
		p, err := env.client.Tasks().List(env.ctx, serviceList(f, limit, after))
		if err != nil {
			env.t.Fatalf("Tasks.List: %v", err)
		}
		for _, t := range p.Items {
			out = append(out, t.Ref)
		}
		if p.Complete {
			return out
		}
		after = p.Next
	}
	env.t.Fatal("Tasks.List never completed")
	return nil
}

// eventsAfter returns the events committed after cursor.
func (env *serviceEnv) eventsAfter(after model.Cursor) []model.Event {
	env.t.Helper()
	var out []model.Event
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		for {
			p, err := r.Events(env.ctx, model.Filter{}, after, 100)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if len(p.Items) == 0 || p.Next == after {
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

func eventKinds(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// taskPayload decodes a task.updated event.
func taskPayload(t *testing.T, ev model.Event) model.TaskUpdatedPayload {
	t.Helper()
	var p model.TaskUpdatedPayload
	if ev.Kind != model.EventTaskUpdated || json.Unmarshal(ev.Payload.Data, &p) != nil {
		t.Fatalf("event %s is no task.updated", ev.Kind)
	}
	return p
}

// createdPayload decodes a message.created event.
func createdPayload(t *testing.T, ev model.Event) model.MessageCreatedPayload {
	t.Helper()
	var p model.MessageCreatedPayload
	if ev.Kind != model.EventMessageCreated || json.Unmarshal(ev.Payload.Data, &p) != nil {
		t.Fatalf("event %s is no message.created", ev.Kind)
	}
	return p
}

func wantTaskRefs(t *testing.T, name string, got []model.TaskRef, want ...model.TaskRef) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func textContent(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}
