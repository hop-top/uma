package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// cascadeSize is the message count of the large-contact cascade case: many
// times one cascade step.
const cascadeSize = 2500

// serviceCascadeLargeContact redacts a contact owning cascadeSize messages
// with Privacy.Redact and Cascade, run by Runtime.Run workers. The first
// engine stops mid-way and a second one over the same store finishes: the
// operation completes with every message redacted exactly once and
// nothing another contact wrote touched. Without a clock to advance past
// the stopped worker's lease, one engine runs the whole cascade.
func serviceCascadeLargeContact(t *testing.T, factory StoreFactory) {
	fx := factory(t)
	env := cascadeEngine(t, fx, 0xca5)
	alice, contact := env.seedAuthor("alice", "c1")
	bob, _ := env.seedAuthor("bob", "c1")
	conv := env.conversation(env.inbox, model.ConversationOpen)
	var refs []model.MessageRef
	for len(refs) < cascadeSize {
		env.put(func(tx store.Tx) error {
			for i := 0; i < 500 && len(refs) < cascadeSize; i++ {
				m := env.authored(conv.Ref, alice, len(refs))
				if err := tx.PutMessage(env.ctx, m, ""); err != nil {
					return err
				}
				refs = append(refs, m.Ref)
			}
			return nil
		})
	}
	other := env.authored(conv.Ref, bob, -1)
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, other, "") })

	a, err := env.client.Privacy().Redact(env.ctx, model.Redact{
		Command: env.cmd(contact.Revision), Target: contact.Ref.Ref(), Cascade: true,
	})
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if a.Operation.State != model.Queued {
		t.Fatalf("acceptance = %+v, want a queued cascade", a)
	}
	next := env
	if fx.Advance != nil {
		env.runUntil(a.Operation.Ref, func(op model.Operation) bool { return op.Progress != nil && op.Progress.Done >= 300 })
		if err := env.engine.Close(env.ctx); err != nil {
			t.Fatal(err)
		}
		// A worker stopped between claim and execution leaves its lease
		// to expire.
		fx.Advance(2 * time.Minute)
		next = cascadeEngine(t, fx, 0xca6)
		next.inbox = env.inbox
	}
	op := next.runUntil(a.Operation.Ref, func(op model.Operation) bool { return op.State != model.Queued })
	if op.State != model.Committed || op.Progress == nil || op.Progress.Done != cascadeSize || op.Failure != nil {
		t.Fatalf("cascade = %+v, want committed with %d done", op, cascadeSize)
	}
	counts := map[model.ID]int{}
	var after model.Cursor
	for {
		var p model.Page[model.Event]
		next.put(func(tx store.Tx) (err error) {
			p, err = tx.Events(next.ctx, model.Filter{Kinds: []string{model.EventMessageUpdated}}, after, 1000)
			return err
		})
		for _, ev := range p.Items {
			counts[ev.Resource.ID]++
		}
		if p.Complete || len(p.Items) == 0 {
			break
		}
		after = p.Next
	}
	next.put(func(tx store.Tx) error {
		for i, ref := range refs {
			m, err := tx.GetMessage(next.ctx, ref)
			if err != nil {
				return err
			}
			if !m.Redacted || len(m.Content.Parts) != 0 || counts[ref.ID] != 1 {
				t.Fatalf("message %d: redacted %v, %d parts, %d events; want one redaction", i, m.Redacted, len(m.Content.Parts), counts[ref.ID])
			}
		}
		m, err := tx.GetMessage(next.ctx, other.Ref)
		if err == nil && m.Redacted {
			t.Error("another contact's message was redacted")
		}
		return err
	})
}

// cascadeEngine is a service env over fx's store whose policy allows
// everything, with its own engine and clock.
func cascadeEngine(t *testing.T, fx StoreFixture, seed uint32) *serviceEnv {
	t.Helper()
	clock := portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: &memberAuthorizer{staff: true}, Clock: clock, IDs: portstest.NewSequenceIDs(seed),
		Adapters: []adapter.Factory{serviceAdapter{}},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	client, err := e.For(model.Principal{Tenant: serviceTenant, Actor: "staff-1"})
	if err != nil {
		t.Fatal(err)
	}
	env := &serviceEnv{
		t: t, ctx: t.Context(), fx: fx, clock: clock, engine: e, st: fx.Store,
		ids: portstest.NewSequenceIDs(seed + 0x100), client: client,
	}
	if seed == 0xca5 {
		env.ch = env.configureChannel()
		env.inbox = env.configureInbox("support")
	}
	return env
}

// seedAuthor stores an identity in the env's inbox linked to a new
// contact and returns both.
func (env *serviceEnv) seedAuthor(source string, rev model.Revision) (model.Identity, model.Contact) {
	env.t.Helper()
	contact := model.Contact{Ref: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, Kind: model.ContactKindPerson, Revision: rev}
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: env.inbox, ID: env.id()}, Contact: contact.Ref,
		Namespace: "conformance.user", SourceID: source, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
	}
	env.put(func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(env.ctx, model.IdentityKey{Inbox: env.inbox, Namespace: id.Namespace, SourceID: source}, id); err != nil {
			return err
		}
		return tx.PutContact(env.ctx, contact, "")
	})
	return id, contact
}

// authored is a public message in conv by id.
func (env *serviceEnv) authored(conv model.ConversationRef, id model.Identity, n int) model.Message {
	return model.Message{
		Ref:      model.MessageRef{Conversation: conv, ID: env.id()},
		Author:   model.Author{Identity: id.Ref, Kind: model.ContactKindPerson},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: fmt.Sprint("message ", n)}}},
		Audience: model.Audience{Visibility: model.Public}, Revision: "m1",
	}
}

// runUntil runs Runtime.Run workers until stop accepts the operation, then
// stops them and returns it.
func (env *serviceEnv) runUntil(ref model.OperationRef, stop func(model.Operation) bool) model.Operation {
	env.t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	done := make(chan error, 1)
	go func() {
		done <- env.engine.Runtime().Run(ctx, call, uma.WorkerOptions{Owner: "worker", Limit: 1, Poll: time.Millisecond})
	}()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		op, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
		if err != nil {
			env.t.Fatalf("Operations.Get: %v", err)
		}
		if stop(op) {
			cancel()
			<-done
			return op
		}
		if time.Now().After(deadline) {
			env.t.Fatalf("cascade stuck at %+v progress %+v", op, op.Progress)
		}
		select {
		case err := <-done:
			env.t.Fatalf("Run returned early: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
