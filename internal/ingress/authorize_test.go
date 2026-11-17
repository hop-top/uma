package ingress_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// receivePolicy allows everything except channel.receive while denied,
// and records every channel.receive request it decides.
type receivePolicy struct {
	denied atomic.Bool
	mu     sync.Mutex
	asked  []policy.Authorization
}

func (p *receivePolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	if a.Action != model.ActionChannelReceive {
		return policy.Decision{Allow: true}, nil
	}
	p.mu.Lock()
	p.asked = append(p.asked, a)
	p.mu.Unlock()
	return policy.Decision{Allow: !p.denied.Load()}, nil
}

func (p *receivePolicy) requests() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]policy.Authorization(nil), p.asked...)
}

func receiveHarness(t *testing.T, opts ...enginetest.HarnessOption) (*harness, *receivePolicy) {
	t.Helper()
	p := &receivePolicy{}
	opts = append([]enginetest.HarnessOption{func(c *engine.Config) { c.Authorizer = p }}, opts...)
	return newHarness(t, chatType, opts...), p
}

// Revoking channel.receive stops an existing binding on its next accept:
// nothing of the ingress is written, and once the grant returns the same
// ingress is accepted as new, not as a duplicate.
func TestIngressDeniedByPolicyWritesNothing(t *testing.T) {
	h, p := receiveHarness(t)
	in := h.Configure(perContact(false), dest)
	before := len(h.Events())
	p.denied.Store(true)
	env := h.Inbound("e-denied", "m-denied", "u1", dest)
	_, err := h.Accept(env)
	requireCode(t, err, model.Unauthorized, "denied by policy")
	if me, ok := err.(*model.Error); !ok || me.Retry != model.NoRetry {
		t.Fatalf("denial %#v, want retry advice none", err)
	}
	switch {
	case len(h.Messages(in.Ref)) != 0:
		t.Fatal("message committed under a denied ingress")
	case len(h.Conversations(in.Ref)) != 0:
		t.Fatal("conversation committed under a denied ingress")
	case len(h.Identities(in.Ref)) != 0 || len(h.Contacts()) != 0:
		t.Fatal("identity or contact committed under a denied ingress")
	case len(h.Events()) != before:
		t.Fatal("events appended under a denied ingress")
	}
	p.denied.Store(false)
	r := h.MustAccept(env)
	if !r.Committed || r.Duplicate {
		t.Fatalf("receipt after the grant returned %+v, want committed, not duplicate", r)
	}
	if len(h.Messages(in.Ref)) != 1 {
		t.Fatal("redelivered ingress not committed once allowed")
	}
}

// A redelivery of an accepted ingress is denied too: the stored receipt is
// not revealed under a revoked grant.
func TestIngressDeniedDuplicate(t *testing.T) {
	h, p := receiveHarness(t)
	h.Configure(perContact(false), dest)
	env := h.Inbound("e1", "m1", "u1", dest)
	h.MustAccept(env)
	p.denied.Store(true)
	_, err := h.Accept(env)
	requireCode(t, err, model.Unauthorized, "")
}

// Every accept asks for the service principal on the channel with the
// resolved inbox as destination.
func TestIngressAuthorizesChannelAndInbox(t *testing.T) {
	h, p := receiveHarness(t)
	in := h.Configure(perContact(false), dest)
	n := len(p.requests())
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	got := p.requests()[n:]
	if len(got) != 1 {
		t.Fatalf("channel.receive asked %d times for one accept, want 1", len(got))
	}
	a := got[0]
	switch {
	case a.Principal != h.Service():
		t.Fatalf("principal %+v, want the service principal", a.Principal)
	case a.Resource != h.Channel.Ref.Ref():
		t.Fatalf("resource %+v, want the channel", a.Resource)
	case a.Destination == nil || *a.Destination != in.Ref.Ref():
		t.Fatalf("destination %+v, want the resolved inbox", a.Destination)
	}
}

// Binding a sink asks for channel.receive on the channel; a denial binds
// nothing.
func TestBindDeniedByPolicy(t *testing.T) {
	h, p := receiveHarness(t)
	p.denied.Store(true)
	_, err := ingress.Bind(h.Ctx, h.E, h.Service(), h.Channel.Ref)
	requireCode(t, err, model.Unauthorized, "denied by policy")
	p.denied.Store(false)
	if _, err := ingress.Bind(h.Ctx, h.E, h.Service(), h.Channel.Ref); err != nil {
		t.Fatalf("Bind after the grant returned: %v", err)
	}
	reqs := p.requests()
	last := reqs[len(reqs)-1]
	if last.Resource != h.Channel.Ref.Ref() || last.Destination != nil || last.Revision != h.Channel.Revision {
		t.Fatalf("bind asked %+v, want the channel at its revision", last)
	}
}

// Staging inbound bytes is ingress too.
func TestStageAttachmentDeniedByPolicy(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	h, p := receiveHarness(t, func(c *engine.Config) { c.Blobs = blobs })
	h.Configure(perContact(false), dest)
	p.denied.Store(true)
	_, err := stager(t, h).StageAttachment(h.Ctx, adapter.StageRequest{Key: "k1", Destination: dest, Metadata: image(0, "", public())}, strings.NewReader("x"))
	requireCode(t, err, model.Unauthorized, "denied by policy")
}

// reconfigureStore changes the inbox between authorization and the write
// unit of the next ingress, once.
type reconfigureStore struct {
	store.Store
	armed atomic.Bool
	inbox model.InboxRef
}

func (s *reconfigureStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	if s.armed.CompareAndSwap(true, false) {
		err := s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
			in, err := tx.GetInbox(ctx, s.inbox)
			if err != nil {
				return err
			}
			prev := in.Revision
			in.Revision = "r-reconfigured"
			return tx.PutInbox(ctx, in, prev)
		})
		if err != nil {
			return err
		}
	}
	return s.Store.Transact(ctx, tenant, fn)
}

// An inbox changed after authorization is reauthorized before the write
// unit commits.
func TestIngressReauthorizesChangedInbox(t *testing.T) {
	var rs *reconfigureStore
	h, p := receiveHarness(t, func(c *engine.Config) {
		rs = &reconfigureStore{Store: c.Store}
		c.Store = rs
	})
	in := h.Configure(perContact(false), dest)
	rs.inbox = in.Ref
	n := len(p.requests())
	rs.armed.Store(true)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	if got := len(p.requests()) - n; got != 2 {
		t.Fatalf("channel.receive asked %d times, want 2: once per attempt", got)
	}
	if len(h.Messages(in.Ref)) != 1 {
		t.Fatal("reauthorized ingress not committed")
	}
}
