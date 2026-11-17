package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Agent tasks: the lookup by core reference, the status order and its
// cursor, owner scoping, a task's message history and the replies
// answering a message.
var taskCases = []storeCase{
	{"TaskPutGetByReference", func(t *testing.T, e *env) {
		owner := e.ownerIdentity(t)
		ref := model.TaskRef{Conversation: e.putAgentConversation(t, &owner), ID: e.id(t)}
		in, want := e.fullTask(ref, "r1"), e.fullTask(ref, "r1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, in, "") })
		// The store keeps a snapshot of the caller's value.
		in.Artifacts[0].Content.Parts[0].Text, in.StatusMessage.ID = "changed after put", e.id(t)
		e.wantTask(t, want)
		e.wantTaskByReference(t, e.inbox, want.External, want)
		ext := want.External
		other := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		variants := []struct {
			name string
			ref  model.ExternalRef
		}{
			{"reference in another channel", model.ExternalRef{Channel: other, Namespace: ext.Namespace, Kind: ext.Kind, ID: ext.ID}},
			{"reference in another namespace", model.ExternalRef{Channel: ext.Channel, Namespace: "conformance.other", Kind: ext.Kind, ID: ext.ID}},
			{"reference of another kind", model.ExternalRef{Channel: ext.Channel, Namespace: ext.Namespace, Kind: "message", ID: ext.ID}},
			{"reference with another id", model.ExternalRef{Channel: ext.Channel, Namespace: ext.Namespace, Kind: ext.Kind, ID: ext.ID + "x"}},
			{"reference with a parent id", model.ExternalRef{Channel: ext.Channel, Namespace: ext.Namespace, Kind: ext.Kind, ID: ext.ID, ParentID: "parent"}},
		}
		e.mustView(t, func(r store.Reader) error {
			// Compared field by field: a reference differing in any field
			// names no task.
			for _, v := range variants {
				_, err := r.TaskByReference(e.ctx, e.inbox, v.ref)
				wantCode(t, err, model.NotFound, "TaskByReference of a ", v.name)
			}
			_, err := r.TaskByReference(e.ctx, model.InboxRef{Tenant: tenantA, ID: e.id(t)}, ext)
			wantCode(t, err, model.NotFound)
			_, err = r.TaskByReference(e.ctx, e.inbox, model.ExternalRef{})
			wantCode(t, err, model.Validation)
			return nil
		})
		e.wantForeign(t, func(r store.Reader) error { _, err := r.TaskByReference(e.ctx, e.inbox, ext); return err })
		next := e.fullTask(ref, "r2")
		next.State, next.StatusAt, next.CancelRequested = model.TaskCompleted, next.StatusAt.Add(time.Second), false
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, next, "r1") })
		e.wantTaskByReference(t, e.inbox, ext, next)
	}},
	{"TaskExternalUniqueConflict", func(t *testing.T, e *env) {
		conv := e.putAgentConversation(t, nil)
		a := e.task(model.TaskRef{Conversation: conv, ID: e.id(t)}, model.TaskWorking, e.at(1), "r1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, a, "") })
		b := e.task(model.TaskRef{Conversation: conv, ID: e.id(t)}, model.TaskWorking, e.at(2), "r1")
		b.External = a.External
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutTask(e.ctx, b, "") }), model.Conflict)
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetTask(e.ctx, b.Ref)
			wantCode(t, err, model.NotFound)
			return nil
		})
		// Unique per inbox: another inbox may hold the same reference.
		elsewhere := e.task(model.TaskRef{Conversation: e.putAgentConversationIn(t, e.putInbox(t), nil), ID: e.id(t)},
			model.TaskWorking, e.at(3), "r1")
		elsewhere.External = a.External
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, elsewhere, "") })
		// A zero External is never unique.
		for range 2 {
			z := e.task(model.TaskRef{Conversation: conv, ID: e.id(t)}, model.TaskWorking, e.at(4), "r1")
			z.External = model.ExternalRef{}
			e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, z, "") })
		}
		// Moving a task to another reference frees the old one.
		moved := a
		moved.External, moved.Revision = e.externalRef(model.RefKindTask, "moved"), "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, moved, "r1") })
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, b, "") })
		e.wantTaskByReference(t, e.inbox, a.External, b)
		e.wantTaskByReference(t, e.inbox, moved.External, moved)
		e.wantTaskByReference(t, elsewhere.Ref.Conversation.Inbox, a.External, elsewhere)
	}},
	{"TaskRequiresConversation", func(t *testing.T, e *env) {
		missing := model.TaskRef{Conversation: model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, ID: e.id(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutTask(e.ctx, e.task(missing, model.TaskSubmitted, e.at(1), "r1"), "")
		}), model.NotFound)
		foreign := e.task(model.TaskRef{Conversation: e.putAgentConversation(t, nil), ID: e.id(t)}, model.TaskSubmitted, e.at(1), "r1")
		foreign.External.Channel.Tenant = tenantB
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutTask(e.ctx, foreign, "") }), model.NotFound)
	}},
	{"TasksStatusOrderPagination", func(t *testing.T, e *env) {
		conv := e.putAgentConversation(t, nil)
		// Insertion order is not status order; two tasks share a status
		// time and order by ID, descending; nanoseconds count.
		offsets := []time.Duration{2 * time.Second, 5 * time.Nanosecond, 3 * time.Second, 2 * time.Second}
		refs := make([]model.TaskRef, len(offsets))
		for i, d := range offsets {
			refs[i] = e.putTask(t, conv, model.TaskWorking, e.at(0).Add(d))
		}
		req := model.ListRequest{Page: page(2)}
		first := e.tasksPage(t, req)
		wantRefs(t, "first page", taskRefs(first.Items), refs[2], refs[3])
		if first.Complete {
			t.Fatal("first page complete with tasks left")
		}
		// Tasks written after the page was read never shift the next one:
		// a newer task sorts before the cursor, an older one after the rest.
		newer := e.putTask(t, conv, model.TaskWorking, e.at(0).Add(10*time.Second))
		older := e.putTask(t, conv, model.TaskWorking, e.at(0).Add(time.Nanosecond))
		req.Page.After = first.Next
		second := e.tasksPage(t, req)
		wantRefs(t, "second page", taskRefs(second.Items), refs[0], refs[1])
		req.Page.After = second.Next
		third := e.tasksPage(t, req)
		wantRefs(t, "third page", taskRefs(third.Items), older)
		if !third.Complete {
			t.Error("last page not complete")
		}
		req.Page.After = third.Next
		if end := e.tasksPage(t, req); len(end.Items) != 0 || !end.Complete || end.Next != third.Next {
			t.Errorf("page past the end = %+v, want empty, complete, cursor kept", end)
		}
		wantRefs(t, "all tasks", e.listTasks(t, model.Filter{}), newer, refs[2], refs[3], refs[0], refs[1], older)
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Tasks(e.ctx, model.ListRequest{Page: model.PageRequest{Limit: 2, After: "bogus"}})
			wantCode(t, err, model.Validation)
			return nil
		})
		// A status change moves the task.
		moved := e.task(refs[1], model.TaskCompleted, e.at(20), "r2")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutTask(e.ctx, moved, "r1") })
		wantRefs(t, "after a status change", e.listTasks(t, model.Filter{}), refs[1], newer, refs[2], refs[3], refs[0], older)
	}},
	{"TasksFilterOwnerStateScope", func(t *testing.T, e *env) {
		alice, bob := e.ownerIdentity(t), e.ownerIdentity(t)
		ca, cb, cn := e.putAgentConversation(t, &alice), e.putAgentConversation(t, &bob), e.putConversation(t)
		ta := e.putTask(t, ca, model.TaskWorking, e.at(1))
		tb := e.putTask(t, cb, model.TaskInputRequired, e.at(2))
		tn := e.putTask(t, cn, model.TaskWorking, e.at(3))
		wantRefs(t, "alice", e.listTasks(t, model.Filter{Identity: &alice}), ta)
		wantRefs(t, "bob", e.listTasks(t, model.Filter{Identity: &bob}), tb)
		wantRefs(t, "conversation", e.listTasks(t, model.Filter{Conversation: &cb}), tb)
		wantRefs(t, "inbox", e.listTasks(t, model.Filter{Inboxes: []model.InboxRef{e.inbox}}), tn, tb, ta)
		wantRefs(t, "other inbox", e.listTasks(t, model.Filter{Inboxes: []model.InboxRef{e.putInbox(t)}}))
		wantRefs(t, "states", e.listTasks(t, model.Filter{Statuses: []string{model.TaskInputRequired, model.TaskCompleted}}), tb)
		wantRefs(t, "owner and state", e.listTasks(t, model.Filter{Identity: &bob, Statuses: []string{model.TaskWorking}}))
		// The owner follows the conversation.
		e.mustTx(t, func(tx store.Tx) error {
			c := e.agentConversation(cb, &alice, "r2")
			return tx.PutConversation(e.ctx, c, "r1")
		})
		wantRefs(t, "alice after the move", e.listTasks(t, model.Filter{Identity: &alice}), tb, ta)
		wantRefs(t, "bob after the move", e.listTasks(t, model.Filter{Identity: &bob}))
		e.mustView(t, func(r store.Reader) error {
			for _, f := range []model.Filter{
				{Kinds: []string{"x"}},
				{Contact: &model.ContactRef{Tenant: tenantA, ID: e.id(t)}},
				{Task: &ta},
			} {
				_, err := r.Tasks(e.ctx, model.ListRequest{Filter: f, Page: page(2)})
				wantCode(t, err, model.Validation)
			}
			return nil
		})
	}},
	{"MessagesFilterByTask", func(t *testing.T, e *env) {
		conv, other := e.putAgentConversation(t, nil), e.putAgentConversation(t, nil)
		tk, tk2 := e.putTask(t, conv, model.TaskWorking, e.at(1)), e.putTask(t, conv, model.TaskWorking, e.at(2))
		m1 := e.putLinked(t, conv, &tk, nil)
		m2 := e.putLinked(t, conv, nil, nil)
		e.putLinked(t, conv, &tk2, nil)
		m4 := e.putLinked(t, conv, &tk, nil)
		e.putLinked(t, other, ptr(e.putTask(t, other, model.TaskWorking, e.at(3))), nil)
		wantRefs(t, "task history", e.listMessages(t, model.Filter{Task: &tk}), m1, m4)
		wantRefs(t, "task and conversation", e.listMessages(t, model.Filter{Task: &tk, Conversation: &conv}), m1, m4)
		wantRefs(t, "task and other conversation", e.listMessages(t, model.Filter{Task: &tk, Conversation: &other}))
		wantRefs(t, "task and inbox", e.listMessages(t, model.Filter{Task: &tk, Inboxes: []model.InboxRef{e.inbox}}), m1, m4)
		// Binding and unbinding keep recording order.
		e.mustTx(t, func(tx store.Tx) error {
			bound := e.message(m2, "r2")
			bound.Task = &tk
			unbound := e.message(m1, "r2")
			return firstErr(tx.PutMessage(e.ctx, bound, "r1"), tx.PutMessage(e.ctx, unbound, "r1"))
		})
		wantRefs(t, "after rebinding", e.listMessages(t, model.Filter{Task: &tk}), m2, m4)
		unknown := model.TaskRef{Conversation: conv, ID: e.id(t)}
		wantRefs(t, "unknown task", e.listMessages(t, model.Filter{Task: &unknown}))
	}},
	{"MessageLinksStayInConversation", func(t *testing.T, e *env) {
		conv, other := e.putConversation(t), e.putConversation(t)
		tk := model.TaskRef{Conversation: other, ID: e.id(t)}
		parent := model.MessageRef{Conversation: other, ID: e.id(t)}
		for _, c := range []struct {
			name string
			msg  model.Message
		}{
			{"a task of another conversation", e.linked(t, conv, &tk, nil)},
			{"a parent in another conversation", e.linked(t, conv, nil, &parent)},
		} {
			wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutMessage(e.ctx, c.msg, "") }), model.Validation, "PutMessage linked to ", c.name)
		}
	}},
	{"RepliesByParent", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		p, q := e.putLinked(t, conv, nil, nil), e.putLinked(t, conv, nil, nil)
		r1 := e.putLinked(t, conv, nil, &p)
		r2 := e.putLinked(t, conv, nil, &q)
		r3 := e.putLinked(t, conv, nil, &p)
		wantRefs(t, "replies to p", e.replies(t, p), r1, r3)
		wantRefs(t, "replies to q", e.replies(t, q), r2)
		wantRefs(t, "replies to a reply", e.replies(t, r1))
		wantRefs(t, "replies to an unknown message", e.replies(t, model.MessageRef{Conversation: conv, ID: e.id(t)}))
		e.mustTx(t, func(tx store.Tx) error {
			m := e.linked(t, conv, nil, &p)
			m.Ref, m.Revision = r2, "r2"
			return tx.PutMessage(e.ctx, m, "r1")
		})
		wantRefs(t, "after re-parenting", e.replies(t, p), r1, r2, r3)
		// A tombstone keeps its parent.
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Message: &r1, Expected: "r1", Revision: "r2"})
			return err
		})
		wantRefs(t, "after redaction", e.replies(t, p), r1, r2, r3)
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Replies(e.ctx, model.MessageRef{}, page(1))
			wantCode(t, err, model.Validation)
			return nil
		})
		e.wantForeign(t, func(r store.Reader) error { _, err := r.Replies(e.ctx, p, page(1)); return err })
	}},
	{"IngressReceiptRoundTripsResultRefs", func(t *testing.T, e *env) {
		conv := e.putAgentConversation(t, nil)
		msg, tk := model.MessageRef{Conversation: conv, ID: e.id(t)}, model.TaskRef{Conversation: conv, ID: e.id(t)}
		ctxRef := model.ExternalRef{Channel: e.channel, Namespace: "conformance.context", Kind: model.RefKindContext, ID: "ctx-1"}
		rec := e.ingressRecord(t, "evt-refs")
		if _, created := e.claimIngress(t, rec); !created {
			t.Fatal("first claim not created")
		}
		rc := model.IngressReceipt{ID: rec.ID, Durable: true, Committed: true, Conversation: &conv, Context: &ctxRef, Message: &msg, Task: &tk}
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, rec.ID, rc) })
		// An equal receipt in fresh memory is the same receipt.
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, rec.ID, rc.Clone()) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Ingress(e.ctx, rec.ID)
			if err == nil && !reflect.DeepEqual(got.Receipt, rc) {
				t.Errorf("stored receipt = %+v, want %+v", got.Receipt, rc)
			}
			return err
		})
		if got, created := e.claimIngress(t, e.ingressRecord(t, "evt-refs")); created || !reflect.DeepEqual(got.Receipt, rc) {
			t.Errorf("duplicate claim = %+v created=%v, want the stored receipt %+v", got.Receipt, created, rc)
		}
		otherTask := model.TaskRef{Conversation: conv, ID: e.id(t)}
		changed := rc.Clone()
		changed.Task = &otherTask
		for _, c := range []struct {
			name    string
			receipt model.IngressReceipt
		}{
			{"a task link that differs", changed},
			{"no links", model.IngressReceipt{ID: rec.ID, Durable: true, Committed: true}},
		} {
			wantCode(t, e.txErr(func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, rec.ID, c.receipt) }), model.Conflict, "CompleteIngress with a receipt of ", c.name)
		}
		fresh := e.ingressRecord(t, "evt-bad-refs")
		e.claimIngress(t, fresh)
		uncommitted := model.IngressReceipt{ID: fresh.ID, Conversation: &conv}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, fresh.ID, uncommitted) }), model.Validation)
		foreignConv := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenantB, ID: e.id(t)}, ID: e.id(t)}
		foreign := model.IngressReceipt{ID: fresh.ID, Committed: true, Conversation: &foreignConv}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, fresh.ID, foreign) }), model.NotFound)
	}},
	{"TasksTenantIsolation", func(t *testing.T, e *env) {
		// Tenant B mirrors tenant A's IDs exactly: only the tenant tells
		// the rows apart, so every read must scope by it.
		owner := e.ownerIdentity(t)
		conv := e.putAgentConversation(t, &owner)
		tk := e.putTask(t, conv, model.TaskWorking, e.at(1))
		msg := e.putLinked(t, conv, &tk, nil)
		reply := e.putLinked(t, conv, nil, &msg)
		mirror := e.mirrorTenant(t, owner, tk, msg, reply)
		wantRefs(t, "tenant A tasks", e.listTasks(t, model.Filter{}), tk)
		wantRefs(t, "tenant A owner", e.listTasks(t, model.Filter{Identity: &owner}), tk)
		wantRefs(t, "tenant A task history", e.listMessages(t, model.Filter{Task: &tk}), msg)
		wantRefs(t, "tenant A replies", e.replies(t, msg), reply)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.TaskByReference(e.ctx, e.inbox, model.TaskExternalRef(e.channel, tk.ID))
			if err == nil && got.Ref != tk {
				t.Errorf("task by reference = %v, want tenant A's %v", got.Ref, tk)
			}
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			_, err := r.TaskByReference(e.ctx, mirror.task.Conversation.Inbox, mirror.external)
			wantCode(t, err, model.NotFound)
			// Tenant B's task under tenant A's own inbox and channel IDs.
			_, err = r.TaskByReference(e.ctx, e.inbox, model.TaskExternalRef(e.channel, mirror.only))
			wantCode(t, err, model.NotFound)
			_, err = r.Tasks(e.ctx, model.ListRequest{Filter: model.Filter{Identity: &mirror.owner}, Page: page(2)})
			wantCode(t, err, model.NotFound)
			_, err = r.Messages(e.ctx, model.ListRequest{Filter: model.Filter{Task: &mirror.task}, Page: page(2)})
			wantCode(t, err, model.NotFound)
			_, err = r.Replies(e.ctx, mirror.message, page(2))
			wantCode(t, err, model.NotFound)
			return nil
		})
	}},
}

func ptr[T any](v T) *T { return &v }
