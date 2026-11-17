package services_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// mutationPolicy allows everything except the denied actions and reading
// the hidden resource.
type mutationPolicy struct {
	mu     sync.Mutex
	deny   map[string]bool
	hidden model.Ref
	checks []policy.Authorization
}

func (p *mutationPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, a)
	if p.deny[a.Action] || (a.Action == model.ActionMessageRead && a.Resource == p.hidden) {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (p *mutationPolicy) denyAction(action string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deny == nil {
		p.deny = map[string]bool{}
	}
	p.deny[action] = true
}

func (p *mutationPolicy) hide(r model.Ref) {
	p.mu.Lock()
	p.hidden = r
	p.mu.Unlock()
}

// last returns the latest check of action.
func (p *mutationPolicy) last(action string) (policy.Authorization, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.checks) - 1; i >= 0; i-- {
		if p.checks[i].Action == action {
			return p.checks[i], true
		}
	}
	return policy.Authorization{}, false
}

// mutationWorld is a rig whose adapter declares edit, delete and react,
// seeded with an inbound public message holding its provider message
// reference on the route channel (target), an edited one, a redacted and
// a deleted one, and public messages in another inbox and another tenant.
type mutationWorld struct {
	*rig
	policy                  *mutationPolicy
	target, edited          model.Message
	redacted, deleted       model.Message
	otherInbox, otherTenant model.Message
	providerRef             model.ExternalRef
	n                       int
}

func newMutationWorld(t *testing.T, opts ...enginetest.RigOption) *mutationWorld {
	t.Helper()
	p := &mutationPolicy{}
	opts = append([]enginetest.RigOption{func(c *engine.Config, _ *outFactory) { c.Authorizer = p }}, opts...)
	r := newRig(t, opts...)
	w := &mutationWorld{rig: r, policy: p}
	id := func(n int) model.ID { return model.ID(fmt.Sprintf("00000004-0000-7000-8000-%012x", n)) }
	msg := func(conv model.ConversationRef, n int) model.Message {
		return model.Message{
			Ref: model.MessageRef{Conversation: conv, ID: id(n)}, Audience: model.Audience{Visibility: model.Public},
			Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "original " + string(id(n))}}},
			RecordedAt: r.Clock.Now(), Revision: "m1",
		}
	}
	w.providerRef = model.ProviderMessageRef(r.Ch.Ref, "provider-msg-1")
	w.target = msg(r.Conv.Ref, 1)
	w.target.External = []model.ExternalRef{w.providerRef}
	w.edited = msg(r.Conv.Ref, 2)
	op := model.OperationRef{Tenant: tenantA, ID: id(90)}
	w.edited.LastEdit = &model.Edit{Actor: "editor-1", At: r.Clock.Now().Add(-time.Hour).UTC(), Operation: &op}
	w.redacted = msg(r.Conv.Ref, 3)
	w.redacted.Redacted, w.redacted.Content = true, model.Content{}
	w.deleted = msg(r.Conv.Ref, 4)
	w.deleted.Deleted, w.deleted.Content = true, model.Content{}
	inbox2 := model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: inboxTwo}, Channel: r.Ch.Ref, Name: "two", Revision: "in"}
	conv2 := model.Conversation{Ref: model.ConversationRef{Inbox: inbox2.Ref, ID: id(5)}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv"}
	w.otherInbox = msg(conv2.Ref, 6)
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error {
		return engine.FirstErr(
			tx.PutMessage(ctx, w.target, ""), tx.PutMessage(ctx, w.edited, ""),
			tx.PutMessage(ctx, w.redacted, ""), tx.PutMessage(ctx, w.deleted, ""),
			tx.PutInbox(ctx, inbox2, ""), tx.PutConversation(ctx, conv2, ""), tx.PutMessage(ctx, w.otherInbox, ""),
		)
	})
	chB := model.Channel{Ref: model.ChannelRef{Tenant: tenantB, ID: chanOne}, Type: outKey, Revision: "c"}
	inB := model.Inbox{Ref: model.InboxRef{Tenant: tenantB, ID: inboxOne}, Channel: chB.Ref, Revision: "i"}
	convB := model.Conversation{Ref: model.ConversationRef{Inbox: inB.Ref, ID: convOne}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv"}
	w.otherTenant = msg(convB.Ref, 7)
	w.seed(tenantB, func(ctx context.Context, tx store.Tx) error {
		return engine.FirstErr(tx.PutChannel(ctx, chB, ""), tx.PutInbox(ctx, inB, ""),
			tx.PutConversation(ctx, convB, ""), tx.PutMessage(ctx, w.otherTenant, ""))
	})
	return w
}

func (w *mutationWorld) seed(tenant model.TenantID, fn func(context.Context, store.Tx) error) {
	w.T.Helper()
	if err := w.E.Store().Transact(w.Ctx, tenant, func(tx store.Tx) error { return fn(w.Ctx, tx) }); err != nil {
		w.T.Fatalf("seed: %v", err)
	}
}

// cmd returns a fresh command at rev.
func (w *mutationWorld) cmd(rev model.Revision) model.Command {
	w.n++
	return model.Command{Call: model.Call{Correlation: "corr-mut"}, Operation: model.OperationID(fmt.Sprintf("op-mut-%d", w.n)), ExpectedRevision: rev}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func (w *mutationWorld) edit(m model.Message, cmd model.Command, s string) (model.Acceptance, error) {
	return w.C.Messages().Edit(w.Ctx, model.EditMessage{Command: cmd, Message: m.Ref, Content: text(s)})
}

func (w *mutationWorld) remove(m model.Message, cmd model.Command) (model.Acceptance, error) {
	return w.C.Messages().Delete(w.Ctx, model.DeleteMessage{Command: cmd, Message: m.Ref})
}

func (w *mutationWorld) react(m model.Message, cmd model.Command, emoji string, remove bool) (model.Acceptance, error) {
	return w.C.Messages().React(w.Ctx, model.Reaction{Command: cmd, Message: m.Ref, Emoji: emoji, Remove: remove})
}

// get reads an operation through Operations.Get.
func (w *mutationWorld) get(ref model.OperationRef) model.Operation {
	w.T.Helper()
	op, err := w.C.Operations().Get(w.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		w.T.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// record reads an operation's stored record.
func (w *mutationWorld) record(ref model.OperationRef) store.OperationRecord {
	w.T.Helper()
	var rec store.OperationRecord
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		rec, err = r.Operation(w.Ctx, ref)
		return err
	}); err != nil {
		w.T.Fatal(err)
	}
	return rec
}

// message reads a stored message.
func (w *mutationWorld) message(ref model.MessageRef) model.Message {
	w.T.Helper()
	var m model.Message
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		m, err = r.GetMessage(w.Ctx, ref)
		return err
	}); err != nil {
		w.T.Fatal(err)
	}
	return m
}

// settle moves an operation to state as an executor would.
func (w *mutationWorld) settle(ref model.OperationRef, state model.DeliveryState) {
	w.T.Helper()
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error {
		rec, err := tx.Operation(ctx, ref)
		if err != nil {
			return err
		}
		next := rec.Operation.Clone()
		next.State, next.Revision = state, rec.Operation.Revision+"-settled"
		return tx.UpdateOperation(ctx, next, rec.Operation.Revision, store.Fence{})
	})
}

// requireNothingQueued fails when any operation work was queued.
func (w *mutationWorld) requireNothingQueued(t *testing.T) {
	t.Helper()
	if n := len(w.Claim("w")); n != 0 {
		t.Fatalf("refused mutations queued %d work items", n)
	}
}
