package conformance

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Agent ingress fault cases: the unit that opens a task for a message and
// completes the ingress receipt with its references commits whole or not
// at all. A retry after a crash then either finds nothing, or finds the
// first acceptance whole.

const faultAgentDest = "agents@example.test"

// agentInbox adds a per-agent-context inbox to the env's channel.
func (f *faultEnv) agentInbox(t *testing.T) model.InboxRef {
	t.Helper()
	in := model.InboxRef{Tenant: tenantA, ID: f.id(t)}
	f.mustTx(t, func(tx store.Tx) error {
		return tx.PutInbox(f.ctx, model.Inbox{
			Ref: in, Channel: f.ch, Name: "agents", Destinations: []string{faultAgentDest},
			Selection: model.Selection{Mode: model.ModePerAgentContext}, Revision: "in2",
		}, "")
	})
	return in
}

// agentMessage is an initial request of a remote agent that opens a task,
// into a context of the agent's own allocation.
func (f *faultEnv) agentMessage(event, ctx string) model.Ingress {
	return model.Ingress{Call: model.Call{Correlation: "rx-" + model.CorrelationID(event)}, Envelope: model.Envelope{
		MessageID: "m-" + event, Destination: faultAgentDest, Kind: model.EnvelopeMessage,
		Sender:     model.ExternalRef{Channel: f.ch, Namespace: faultNS, Kind: "user", ID: "agent-1"},
		SenderKind: "", Assurance: "verified",
		References: []model.ExternalRef{{Channel: f.ch, Namespace: faultNS, Kind: model.RefKindContext, ID: ctx}},
		Task:       &model.TaskBinding{Open: true},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello " + event}}},
		Audience:   model.Audience{Visibility: model.Public}, OccurredAt: f.fx.Now(),
	}}
}

// agentRecords counts what the agent inbox holds.
type agentRecords struct {
	conversations []model.Conversation
	messages      []model.Message
	tasks         []model.Task
	events        int
}

func (f *faultEnv) agentRecords(t *testing.T, in model.InboxRef) agentRecords {
	t.Helper()
	var out agentRecords
	f.mustView(t, func(r store.Reader) error {
		req := model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: page(100)}
		c, err := r.Conversations(f.ctx, req)
		if err != nil {
			return err
		}
		m, err := r.Messages(f.ctx, req)
		if err != nil {
			return err
		}
		k, err := r.Tasks(f.ctx, req)
		if err != nil {
			return err
		}
		out.conversations, out.messages, out.tasks = c.Items, m.Items, k.Items
		return nil
	})
	for _, ev := range f.allEvents(t, model.Filter{Kinds: []string{model.EventMessageReceived, model.EventTaskUpdated}}, 50) {
		if ev.Inbox == in.ID {
			out.events++
		}
	}
	return out
}

// wantAccepted checks the inbox holds exactly the first acceptance: one
// conversation of context ctx, its message and its task, one
// message.received and one task.updated, and that r names them.
func (f *faultEnv) wantAccepted(t *testing.T, in model.InboxRef, ctx string, r model.IngressReceipt) {
	t.Helper()
	got := f.agentRecords(t, in)
	if len(got.conversations) != 1 || len(got.messages) != 1 || len(got.tasks) != 1 || got.events != 2 {
		t.Fatalf("records = %d conversations, %d messages, %d tasks, %d events; want 1, 1, 1 and 2",
			len(got.conversations), len(got.messages), len(got.tasks), got.events)
	}
	c, m, k := got.conversations[0], got.messages[0], got.tasks[0]
	if c.ReplyRoute.ID != ctx || m.Task == nil || *m.Task != k.Ref || k.Ref.Conversation != c.Ref {
		t.Fatalf("records = conversation %+v, message task %v, task %+v; want context %q bound together", c, m.Task, k, ctx)
	}
	if r.Conversation == nil || r.Context == nil || r.Message == nil || r.Task == nil ||
		*r.Conversation != c.Ref || r.Context.ID != ctx || *r.Message != m.Ref || *r.Task != k.Ref {
		t.Fatalf("receipt = %+v, want the references of the records", r)
	}
}

// An ingress unit that opens a task dies before it commits: no
// conversation, message, task or event was recorded, so the redelivery,
// which brings a context of its own, is accepted whole. The next
// redelivery is a duplicate carrying the references of that acceptance.
func agentIngressCrashBeforeCommit(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	in := f.agentInbox(t)
	p := f.start(t)
	p.store.at(beforeCommit, (*unitLog).claimsIngress, p.store.crash)
	done, _ := p.receive()
	f.push(t, f.agentMessage("evt-1", "ctx-a"))
	if r := f.accept(t); r.err == nil {
		t.Fatalf("ingress = %+v, accepted by a unit that crashed before commit", r.receipt)
	}
	wait(t, done)
	p = f.restart(t, cross)
	if got := f.agentRecords(t, in); len(got.conversations)+len(got.messages)+len(got.tasks)+got.events != 0 {
		t.Fatalf("records after the crash = %+v, want none", got)
	}
	wantCode(t, p.engine.Runtime().Receive(p.ctx, faultCall(), f.ch), model.Conflict)
	e.fx.Advance(leaseExpiry)
	_, stop := p.receive()
	f.push(t, f.agentMessage("evt-1", "ctx-b"))
	first := f.accept(t)
	if first.err != nil || !first.receipt.Committed || first.receipt.Duplicate {
		t.Fatalf("redelivery = %+v, %v; want it committed once", first.receipt, first.err)
	}
	f.wantAccepted(t, in, "ctx-b", first.receipt)
	f.push(t, f.agentMessage("evt-1", "ctx-c"))
	dup := f.accept(t)
	stop()
	if dup.err != nil || !dup.receipt.Duplicate {
		t.Fatalf("second redelivery = %+v, %v; want a duplicate", dup.receipt, dup.err)
	}
	f.wantAccepted(t, in, "ctx-b", dup.receipt)
}

// The unit commits and the process dies before it learns so. Nothing is
// lost and nothing doubled: the redelivery, carrying a new context, is a
// duplicate that names the context, conversation, message and task the
// committed unit recorded.
func agentIngressLostAfterCommit(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	in := f.agentInbox(t)
	p := f.start(t)
	p.store.at(afterCommit, (*unitLog).claimsIngress, p.store.crash)
	done, _ := p.receive()
	f.push(t, f.agentMessage("evt-1", "ctx-a"))
	if r := f.accept(t); r.err == nil {
		t.Fatalf("ingress = %+v, answered by a process that crashed before answering", r.receipt)
	}
	wait(t, done)
	p = f.restart(t, cross)
	got := f.agentRecords(t, in)
	if len(got.conversations) != 1 || len(got.messages) != 1 || len(got.tasks) != 1 || got.events != 2 {
		t.Fatalf("records after the crash = %+v, want the committed unit whole", got)
	}
	wantCode(t, p.engine.Runtime().Receive(p.ctx, faultCall(), f.ch), model.Conflict)
	e.fx.Advance(leaseExpiry)
	_, stop := p.receive()
	f.push(t, f.agentMessage("evt-1", "ctx-b"))
	r := f.accept(t)
	stop()
	if r.err != nil || !r.receipt.Duplicate {
		t.Fatalf("redelivery = %+v, %v; want a duplicate", r.receipt, r.err)
	}
	f.wantAccepted(t, in, "ctx-a", r.receipt)
}
