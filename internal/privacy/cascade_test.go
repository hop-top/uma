package privacy_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/privacy"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

func cascade(target model.Ref, op model.OperationID, rev model.Revision) model.Redact {
	return model.Redact{Command: cmd(op, rev), Target: target, Cascade: true, Reason: "erasure"}
}

func conversation(t *testing.T, r *enginetest.Rig) model.Conversation {
	t.Helper()
	c, err := r.C.Conversations().Get(r.Ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "c"}, Ref: r.Conv.Ref})
	if err != nil {
		t.Fatalf("Conversations.Get: %v", err)
	}
	return c
}

// drive runs every claimable operation item of tenant until none is left
// and returns the number of items it ran.
func drive(t *testing.T, ctx context.Context, rt *runtime.Runtime, tenant model.TenantID) int {
	t.Helper()
	n := 0
	for range 10_000 {
		works, err := rt.ClaimWork(ctx, tenant, runtime.WorkerOptions{
			Owner: "w", Limit: 1, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute,
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(works) == 0 {
			return n
		}
		for _, w := range works {
			if err := rt.ExecuteClaimed(ctx, tenant, w); err != nil {
				t.Fatalf("execute: %v", err)
			}
			n++
		}
	}
	t.Fatal("work never ran out")
	return n
}

func operation(t *testing.T, c enginetest.Client, ctx context.Context, ref model.OperationRef) model.Operation {
	t.Helper()
	op, err := c.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	if err != nil {
		t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

func done(op model.Operation) int64 {
	if op.Progress == nil {
		return -1
	}
	return op.Progress.Done
}

// Redacting a message never touches its neighbours. A conversation
// cascade is a queued operation that, run by the workers, redacts every
// message in it, their attachments' bytes included, with one
// message.updated each, leaves the conversation record and reports
// completion with its progress.
func TestCascadeRedactsAConversation(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	n, _ := note(t, r)
	a := r.Reply("op-reply", secret)
	r.Step("w")
	reply := get(t, r, *a.Operation.ResultMessage)
	other, _ := noteAs(t, r, "-other")

	if _, err := r.C.Privacy().Redact(r.Ctx, redactMessage(other, "op-one")); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if got := get(t, r, n.Ref); got.Redacted || got.Revision != n.Revision {
		t.Fatalf("neighbour after a message redaction = %+v, want it untouched", got)
	}

	before := len(r.Events())
	conv := conversation(t, r)
	res, err := r.C.Privacy().Redact(r.Ctx, cascade(conv.Ref.Ref(), "op-cascade", conv.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if res.Operation.State != model.Queued || res.Operation.Result == nil || *res.Operation.Result != conv.Ref.Ref() {
		t.Fatalf("acceptance = %+v, want a queued operation on the conversation", res)
	}
	if got := get(t, r, n.Ref); got.Redacted {
		t.Fatal("cascade redacted before any worker ran")
	}
	drive(t, r.Ctx, r.RT, enginetest.TenantA)
	op := operation(t, r.C, r.Ctx, res.Operation.Ref)
	if op.State != model.Committed || done(op) != 3 || op.Failure != nil {
		t.Fatalf("cascade operation = %+v, want committed with 3 done", op)
	}
	for _, m := range []model.Message{n, reply, other} {
		if got := get(t, r, m.Ref); !got.Redacted || len(got.Content.Parts) != 0 {
			t.Errorf("message %s after the cascade = %+v, want a tombstone", m.Ref.ID, got)
		}
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Errorf("blobs = %v, want every attachment's bytes deleted", keys)
	}
	evs := r.Events()[before:]
	if len(evs) != 3 {
		t.Fatalf("events = %v, want a message.updated per message", enginetest.EventKinds(evs))
	}
	requireNoSecret(t, r.Events())
	if got := conversation(t, r); got.Revision != conv.Revision || got.Status != conv.Status {
		t.Errorf("conversation after the cascade = %+v, want it unchanged", got)
	}
	again, err := r.C.Privacy().Redact(r.Ctx, cascade(conv.Ref.Ref(), "op-cascade", conv.Revision))
	if err != nil || !again.Replayed || again.Operation.State != model.Committed {
		t.Fatalf("retry = %+v, %v; want the settled cascade replayed", again, err)
	}
}

// seed is an inbox with alice's and bob's identities, one inbound message
// from bob and n more from alice written straight to the store.
type seed struct {
	h          *enginetest.Harness
	inbox      model.InboxRef
	alice, bob model.Identity
	contact    model.Contact
	messages   []model.MessageRef
	bobs       model.Message
}

func seedContact(t *testing.T, n int, opts ...enginetest.HarnessOption) *seed {
	t.Helper()
	h := enginetest.NewHarness(t, enginetest.ChatType, append([]enginetest.HarnessOption{
		func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{} },
	}, opts...)...)
	s := &seed{h: h, inbox: h.Configure(enginetest.PerContact(false), enginetest.Dest).Ref}
	h.MustAccept(h.Inbound("e-alice", "pm-alice", "alice", enginetest.Dest))
	h.MustAccept(h.Inbound("e-bob", "pm-bob", "bob", enginetest.Dest))
	first := h.MessageByProviderID(s.inbox, "pm-alice")
	s.bobs = h.MessageByProviderID(s.inbox, "pm-bob")
	s.messages = append(s.messages, first.Ref)
	h.View(func(r store.Reader) (err error) {
		if s.alice, err = r.GetIdentity(h.Ctx, first.Author.Identity); err != nil {
			return err
		}
		if s.bob, err = r.GetIdentity(h.Ctx, s.bobs.Author.Identity); err != nil {
			return err
		}
		s.contact, err = r.GetContact(h.Ctx, s.alice.Contact)
		return err
	})
	for len(s.messages) < n {
		h.Tx(func(tx store.Tx) error {
			for i := 0; i < 500 && len(s.messages) < n; i++ {
				m := first.Clone()
				m.Ref.ID = h.ID()
				m.External = nil
				m.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: secret + fmt.Sprint(len(s.messages))}}}
				m.Revision = "seeded"
				if err := tx.PutMessage(h.Ctx, m, ""); err != nil {
					return err
				}
				s.messages = append(s.messages, m.Ref)
			}
			return nil
		})
	}
	return s
}

func (s *seed) message(t *testing.T, ref model.MessageRef) model.Message {
	t.Helper()
	var m model.Message
	s.h.View(func(r store.Reader) (err error) {
		m, err = r.GetMessage(s.h.Ctx, ref)
		return err
	})
	return m
}

// redactions counts message.updated events per redacted message.
func redactions(evs []model.Event) map[model.ID]int {
	out := map[model.ID]int{}
	for _, ev := range evs {
		if ev.Kind == model.EventMessageUpdated {
			out[ev.Resource.ID]++
		}
	}
	return out
}

// events reads the whole event log.
func (s *seed) events(t *testing.T) []model.Event {
	t.Helper()
	var out []model.Event
	var after model.Cursor
	for {
		var p model.Page[model.Event]
		s.h.View(func(r store.Reader) (err error) {
			p, err = r.Events(s.h.Ctx, model.Filter{}, after, 1000)
			return err
		})
		out = append(out, p.Items...)
		if p.Complete || len(p.Items) == 0 {
			return out
		}
		after = p.Next
	}
}

func (s *seed) requireRedacted(t *testing.T, upTo int) {
	t.Helper()
	counts := redactions(s.events(t))
	for i, ref := range s.messages {
		m := s.message(t, ref)
		want := i < upTo
		if m.Redacted != want || (want && len(m.Content.Parts) != 0) {
			t.Fatalf("message %d redacted = %v, want %v", i, m.Redacted, want)
		}
		if want && counts[ref.ID] != 1 {
			t.Fatalf("message %d redacted %d times, want once", i, counts[ref.ID])
		}
	}
	if m := s.message(t, s.bobs.Ref); m.Redacted {
		t.Fatal("bob's message was redacted")
	}
}

// A contact cascade completes whatever the contact's size: 2500 messages
// in bounded batches, each redacted once, and nothing another contact
// wrote. An identity cascade is the same for one identity.
func TestCascadeRedactsALargeContact(t *testing.T) {
	s := seedContact(t, 2500)
	h := s.h
	a, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.contact.Ref.Ref(), "op-cascade", s.contact.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	steps := drive(t, h.Ctx, h.RT, enginetest.TenantA)
	op := operation(t, h.Client, h.Ctx, a.Operation.Ref)
	if op.State != model.Committed || done(op) != 2500 {
		t.Fatalf("cascade operation = %+v, want committed with 2500 done", op)
	}
	if steps < 2500/100 {
		t.Errorf("cascade ran in %d steps, want bounded batches", steps)
	}
	s.requireRedacted(t, 2500)
	requireNoSecret(t, s.events(t))

	b, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.bob.Ref.Ref(), "op-bob", s.bob.Revision))
	if err != nil {
		t.Fatalf("identity cascade: %v", err)
	}
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	if op := operation(t, h.Client, h.Ctx, b.Operation.Ref); op.State != model.Committed || done(op) != 1 {
		t.Fatalf("identity cascade = %+v", op)
	}
	if m := s.message(t, s.bobs.Ref); !m.Redacted {
		t.Error("bob's message survived his identity cascade")
	}
}

// A crash mid-cascade loses at most an uncommitted step: after a restart,
// and after a worker died holding an item's lease, the cascade resumes
// where it stopped, redacting every message exactly once.
func TestCascadeResumesAfterACrash(t *testing.T) {
	s := seedContact(t, 450)
	h := s.h
	a, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.contact.Ref.Ref(), "op-cascade", s.contact.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	opts := runtime.WorkerOptions{Owner: "w1", Limit: 1, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute}
	for range 2 {
		works, err := h.RT.ClaimWork(h.Ctx, enginetest.TenantA, opts)
		if err != nil || len(works) != 1 {
			t.Fatalf("claim = %d, %v", len(works), err)
		}
		if err := h.RT.ExecuteClaimed(h.Ctx, enginetest.TenantA, works[0]); err != nil {
			t.Fatal(err)
		}
	}
	if d := done(operation(t, h.Client, h.Ctx, a.Operation.Ref)); d != 200 {
		t.Fatalf("done after two steps = %d, want 200", d)
	}
	// The worker dies holding the next item.
	if works, err := h.RT.ClaimWork(h.Ctx, enginetest.TenantA, opts); err != nil || len(works) != 1 {
		t.Fatalf("claim = %d, %v", len(works), err)
	}
	if err := h.E.Close(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(2 * time.Minute)

	e := enginetest.MustNew(t, engine.Config{
		Store: h.St, Authorizer: &enginetest.Allow{}, Adapters: nil, Clock: h.Clock,
		IDs: portstest.NewSequenceIDs(0x2e5), Blobs: &portstest.MemoryBlobs{},
	})
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	rt := runtime.New(e, privacy.CascadeEffector())
	drive(t, h.Ctx, rt, enginetest.TenantA)
	c, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	if op := operation(t, c, h.Ctx, a.Operation.Ref); op.State != model.Committed || done(op) != 450 {
		t.Fatalf("cascade after the restart = %+v, want committed with 450 done", op)
	}
	h.E = e
	s.requireRedacted(t, 450)
}

// refusePolicy allows everything except privacy.redact on one resource.
type refusePolicy struct{ resource model.Ref }

func (p *refusePolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	return policy.Decision{Allow: a.Action != model.ActionPrivacyRedact || a.Resource != p.resource}, nil
}

// Each batch rechecks policy for every message as the originating actor. A
// denial stops the cascade: the operation fails with model.Unauthorized
// and its progress counts exactly the messages redacted before the denied
// one, which are tombstones while it and the rest are untouched.
func TestCascadeDenialReportsWhatWasDone(t *testing.T) {
	pol := &refusePolicy{}
	s := seedContact(t, 260, func(c *engine.Config) { c.Authorizer = pol })
	h := s.h
	pol.resource = s.messages[237].Ref()
	a, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.contact.Ref.Ref(), "op-cascade", s.contact.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	op := operation(t, h.Client, h.Ctx, a.Operation.Ref)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Unauthorized || done(op) != 237 {
		t.Fatalf("cascade operation = %+v, want failed unauthorized after 237", op)
	}
	s.requireRedacted(t, 237)
}

// Submission authorizes the cascade on its target and compares the
// target's revision before anything is queued.
func TestCascadeRefusedUpFront(t *testing.T) {
	pol := &selectivePolicy{internal: map[string]bool{}, deny: map[string]bool{model.ActionPrivacyRedactCascade: true}}
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}), func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = pol })
	n, _ := note(t, r)
	conv := conversation(t, r)
	_, err := r.C.Privacy().Redact(r.Ctx, cascade(conv.Ref.Ref(), "op-1", conv.Revision))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	pol.deny = map[string]bool{}
	_, err = r.C.Privacy().Redact(r.Ctx, cascade(conv.Ref.Ref(), "op-2", "stale"))
	enginetest.RequireCode(t, err, model.Conflict, "")
	if steps := drive(t, r.Ctx, r.RT, enginetest.TenantA); steps != 0 {
		t.Fatalf("refused cascades queued %d items", steps)
	}
	if got := get(t, r, n.Ref); got.Redacted {
		t.Fatal("a refused cascade redacted the note")
	}
}

// A message whose reply is still queued holds the cascade back without
// failing it: once the reply is delivered the cascade completes.
func TestCascadeWaitsForAQueuedReply(t *testing.T) {
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}))
	a := r.Reply("op-reply", secret)
	opts := runtime.WorkerOptions{Owner: "slow", Limit: 1, Kinds: []string{operations.WorkKindOperation}, Lease: time.Hour}
	held, err := r.RT.ClaimWork(r.Ctx, enginetest.TenantA, opts)
	if err != nil || len(held) != 1 {
		t.Fatalf("claim the reply = %d, %v", len(held), err)
	}
	conv := conversation(t, r)
	res, err := r.C.Privacy().Redact(r.Ctx, cascade(conv.Ref.Ref(), "op-cascade", conv.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	drive(t, r.Ctx, r.RT, enginetest.TenantA)
	if op := operation(t, r.C, r.Ctx, res.Operation.Ref); op.State != model.Queued || done(op) > 0 {
		t.Fatalf("cascade while the reply is queued = %+v, want it waiting", op)
	}
	if m := get(t, r, *a.Operation.ResultMessage); m.Redacted {
		t.Fatal("reply redacted while still queued")
	}
	if err := r.RT.ExecuteClaimed(r.Ctx, enginetest.TenantA, held[0]); err != nil {
		t.Fatal(err)
	}
	enginetest.RequireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	r.Clock.Advance(2 * time.Minute)
	drive(t, r.Ctx, r.RT, enginetest.TenantA)
	op := operation(t, r.C, r.Ctx, res.Operation.Ref)
	if op.State != model.Committed || done(op) != 1 {
		t.Fatalf("cascade = %+v, want committed once the reply settled", op)
	}
	if m := get(t, r, *a.Operation.ResultMessage); !m.Redacted {
		t.Fatal("reply not redacted")
	}
}
