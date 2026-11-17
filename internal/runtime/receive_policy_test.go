package runtime_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// receiveGrant allows everything but channel.receive, which it allows on
// the channel (binding) while bind is set and into an inbox (accepting)
// while accept is set.
type receiveGrant struct{ bind, accept atomic.Bool }

func newReceiveGrant() *receiveGrant {
	g := &receiveGrant{}
	g.bind.Store(true)
	g.accept.Store(true)
	return g
}

func (g *receiveGrant) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	switch {
	case a.Action != model.ActionChannelReceive:
		return policy.Decision{Allow: true}, nil
	case a.Destination == nil:
		return policy.Decision{Allow: g.bind.Load()}, nil
	}
	return policy.Decision{Allow: g.accept.Load()}, nil
}

func rxCall(h *harness) runtime.Call {
	return runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "rx"}}
}

// A receiver denied channel.receive never starts: no instance opens and
// no lease is taken.
func TestReceiveDeniedAtStartup(t *testing.T) {
	g := newReceiveGrant()
	a := &cursorAdapter{runs: make(chan struct{}, 4)}
	h := newHarness(t, cursorType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a); c.Authorizer = g })
	h.Configure(enginetest.PerContact(false), enginetest.Dest)
	g.bind.Store(false)
	bounded, stop := context.WithTimeout(h.Ctx, 10*time.Second)
	defer stop()
	err := h.RT.Receive(bounded, rxCall(h), h.Channel.Ref, runtime.ReceiveOptions{})
	requireCode(t, err, model.Unauthorized, "denied by policy")
	if len(a.runs) != 0 {
		t.Fatal("receiver ran under a denied grant")
	}
	g.bind.Store(true)
	st, err := h.RT.ReceiverStatus(h.Ctx, rxCall(h), h.Channel.Ref)
	if err != nil || st.Owner != "" {
		t.Fatalf("receiver status %+v, %v; want no owner", st, err)
	}
}

// Revoking ingress stops a running receiver: its accept is denied, nothing
// is written, the cursor does not advance and Receive returns the denial.
// Once allowed again, a new Receive resumes from the old cursor and the
// ingress commits.
func TestReceiveStopsWhenIngressRevoked(t *testing.T) {
	g := newReceiveGrant()
	a := &cursorAdapter{runs: make(chan struct{}, 4)}
	h := newHarness(t, cursorType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a); c.Authorizer = g })
	in := h.Configure(enginetest.PerContact(false), enginetest.Dest)
	g.accept.Store(false)
	bounded, stop := context.WithTimeout(h.Ctx, 10*time.Second)
	defer stop()
	err := h.RT.Receive(bounded, rxCall(h), h.Channel.Ref, runtime.ReceiveOptions{})
	requireCode(t, err, model.Unauthorized, "denied by policy")
	done := make(chan error, 1)
	if n := len(h.Messages(in.Ref)); n != 0 {
		t.Fatalf("messages = %d under a denied ingress, want none", n)
	}
	g.accept.Store(true)
	ctx, cancel := context.WithCancel(h.Ctx)
	go func() { done <- h.RT.Receive(ctx, rxCall(h), h.Channel.Ref, runtime.ReceiveOptions{}) }()
	for range 2 {
		select {
		case <-a.runs:
		case <-time.After(10 * time.Second):
			t.Fatal("receiver did not run")
		}
	}
	cancel()
	<-done
	a.mu.Lock()
	seen := append([]string(nil), a.seen...)
	a.mu.Unlock()
	if len(seen) != 2 || seen[1] != "" {
		t.Fatalf("cursors seen at start = %q, want the denied run's cursor not committed", seen)
	}
	if n := len(h.Messages(in.Ref)); n != 1 {
		t.Fatalf("messages = %d after the grant returned, want 1", n)
	}
}

// A mounted webhook denies each ingress once revoked, without remounting;
// a new mount is denied outright.
func TestWebhookBindingHonoursRevocation(t *testing.T) {
	g := newReceiveGrant()
	var mu sync.Mutex
	var sink adapter.Sink
	a := &hookAdapter{handler: func(s adapter.Sink) http.Handler {
		mu.Lock()
		sink = s
		mu.Unlock()
		return http.NotFoundHandler()
	}}
	h := newHarness(t, hookType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a); c.Authorizer = g })
	in := h.Configure(enginetest.PerContact(false), enginetest.Dest)
	if _, err := mount(h); err != nil {
		t.Fatal(err)
	}
	g.accept.Store(false)
	g.bind.Store(false)
	mu.Lock()
	s := sink
	mu.Unlock()
	_, err := s.Accept(h.Ctx, model.Ingress{Envelope: h.Inbound("e1", "m1", "u1", enginetest.Dest)})
	requireCode(t, err, model.Unauthorized, "denied by policy")
	if n := len(h.Messages(in.Ref)); n != 0 {
		t.Fatalf("messages = %d under a revoked grant, want none", n)
	}
	_, err = mount(h)
	requireCode(t, err, model.Unauthorized, "denied by policy")
	_, err = ingress.Bind(h.Ctx, h.E, h.Service(), h.Channel.Ref)
	requireCode(t, err, model.Unauthorized, "")
}
