package a2a_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// conversation returns the conversation of context contextID.
func (r *rig) conversation(contextID string) model.Conversation {
	r.t.Helper()
	var out model.Conversation
	err := r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		if err != nil {
			return err
		}
		for _, c := range p.Items {
			if len(c.External) == 1 && c.External[0].ID == contextID {
				out = c
			}
		}
		return nil
	})
	if err != nil || out.Ref.ID == "" {
		r.t.Fatalf("conversation of context %q: %v", contextID, err)
	}
	return out
}

// messages returns the messages of a conversation in recording order.
func (r *rig) messages(conv model.ConversationRef) []model.Message {
	r.t.Helper()
	var out []model.Message
	err := r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.t.Context(), model.ListRequest{Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// waitMessages waits until the conversation of contextID holds n messages.
func (r *rig) waitMessages(contextID string, n int) (model.Conversation, []model.Message) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var conv model.Conversation
		found := false
		_ = r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
			p, err := rd.Conversations(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
			if err != nil {
				return nil
			}
			for _, c := range p.Items {
				if contextID == "" || len(c.External) == 1 && c.External[0].ID == contextID {
					conv, found = c, true
				}
			}
			return nil
		})
		if found {
			if msgs := r.messages(conv.Ref); len(msgs) >= n {
				return conv, msgs
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("conversation of context %q never held %d messages", contextID, n)
	return model.Conversation{}, nil
}

// hostReply replies to message m as the host.
func (r *rig) hostReply(conv model.ConversationRef, m model.Message, s string) {
	r.t.Helper()
	ref := m.Ref
	if _, err := r.client.Messages().Reply(r.t.Context(), model.Reply{
		Command: r.op(), Conversation: conv, InReplyTo: &ref, Content: text(s),
	}); err != nil {
		r.t.Fatalf("Reply: %v", err)
	}
}

// hostOpen opens a task for message m as the host.
func (r *rig) hostOpen(conv model.ConversationRef, m model.Message, state string) {
	r.t.Helper()
	data, _ := json.Marshal(model.TaskOpen{Message: m.Ref, State: state})
	if _, err := r.client.Tasks().Act(r.t.Context(), model.TaskAction{
		Command: r.op(), Target: model.TaskRef{Conversation: conv},
		Action: model.Extension{Name: model.SchemaTaskOpen, Version: 1, Data: data},
	}); err != nil {
		r.t.Fatalf("Tasks.Act open: %v", err)
	}
}

// hostUpdate reports progress on the task with wire ID taskID.
func (r *rig) hostUpdate(taskID string, u model.TaskUpdate) {
	r.t.Helper()
	// A status change counts as an answer only when recorded after the
	// message, so the fixed clock moves on.
	r.clock.Advance(time.Second)
	t := r.taskByWireID(taskID)
	data, _ := json.Marshal(u)
	cmd := r.op()
	cmd.ExpectedRevision = t.Revision
	if _, err := r.client.Tasks().Act(r.t.Context(), model.TaskAction{
		Command: cmd, Target: t.Ref,
		Action: model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: data},
	}); err != nil {
		r.t.Fatalf("Tasks.Act update: %v", err)
	}
}

func (r *rig) taskByWireID(id string) model.Task {
	r.t.Helper()
	var out model.Task
	err := r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Tasks(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		if err != nil {
			return err
		}
		for _, t := range p.Items {
			if t.External.ID == id {
				out = t
			}
		}
		return nil
	})
	if err != nil || out.Ref.ID == "" {
		r.t.Fatalf("task %q: %v", id, err)
	}
	return out
}

func (r *rig) taskCount() int {
	r.t.Helper()
	n := 0
	_ = r.st.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Tasks(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		n = len(p.Items)
		return err
	})
	return n
}

// async runs f in the background and returns its reply.
func async(f func() reply) <-chan reply {
	ch := make(chan reply, 1)
	go func() { ch <- f() }()
	return ch
}

func await(t *testing.T, ch <-chan reply) reply {
	t.Helper()
	select {
	case rp := <-ch:
		return rp
	case <-time.After(10 * time.Second):
		t.Fatal("request did not finish")
		return reply{}
	}
}
