package runtime_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

const cursorType model.TypeKey = "com.example.ingress.cursor"

// cursorAdapter's receiver reads the sink's cursor, accepts one message
// carrying the next cursor and waits to be stopped.
type cursorAdapter struct {
	mu   sync.Mutex
	seen []string
	runs chan struct{}
}

func (a *cursorAdapter) Describe() adapter.Descriptor {
	return enginetest.IngressAdapter{Typ: cursorType}.Describe()
}
func (*cursorAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *cursorAdapter) Open(_ context.Context, ch model.Channel, _ adapter.Dependencies) (adapter.Instance, error) {
	return &cursorReceiver{a: a, ch: ch.Ref}, nil
}

type cursorReceiver struct {
	a  *cursorAdapter
	ch model.ChannelRef
}

func (*cursorReceiver) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}}, nil
}
func (*cursorReceiver) Close(context.Context) error { return nil }
func (r *cursorReceiver) Run(ctx context.Context, s adapter.Sink) error {
	cs, ok := s.(adapter.CursorSink)
	cursor := "<no cursor sink>"
	if ok {
		var err error
		if cursor, err = cs.ReceiverCursor(ctx); err != nil {
			return err
		}
	}
	r.a.mu.Lock()
	r.a.seen = append(r.a.seen, cursor)
	n := len(r.a.seen)
	r.a.mu.Unlock()
	id := strconv.Itoa(n)
	_, err := s.Accept(ctx, model.Ingress{
		Envelope: model.Envelope{
			EventID: "e" + id, MessageID: "m" + id, Destination: enginetest.Dest, Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: r.ch, Namespace: ingressNS, Kind: "user", ID: "u1"},
			Assurance: "verified", Audience: model.Audience{Visibility: model.Public},
			Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi"}}}, OccurredAt: enginetest.T0,
		},
		ReceiverCursor: "page-" + id,
	})
	r.a.runs <- struct{}{}
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// A restarted receiver resumes from the cursor its last accepted ingress
// carried: the sink it runs with exposes it.
func TestReceiverResumesFromItsCursor(t *testing.T) {
	a := &cursorAdapter{runs: make(chan struct{}, 4)}
	h := newHarness(t, cursorType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a) })
	h.Configure(enginetest.PerContact(false), enginetest.Dest)
	for range 2 {
		ctx, cancel := context.WithCancel(h.Ctx)
		done := make(chan error, 1)
		go func() {
			done <- h.RT.Receive(ctx, runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "rx"}}, h.Channel.Ref, runtime.ReceiveOptions{})
		}()
		select {
		case <-a.runs:
		case <-time.After(10 * time.Second):
			t.Fatal("receiver did not run")
		}
		cancel()
		if err := <-done; err != nil {
			t.Logf("Receive: %v", err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seen) != 2 || a.seen[0] != "" || a.seen[1] != "page-1" {
		t.Fatalf("cursors seen at start = %q, want [\"\" \"page-1\"]", a.seen)
	}
}

// A receiver's status names the owner a Receive call gave and when its
// lease expires, without a competing Receive having to conflict; a channel
// no Receive owns reports no owner.
func TestReceiverStatusReportsOwnerAndExpiry(t *testing.T) {
	a := &cursorAdapter{runs: make(chan struct{}, 4)}
	h := newHarness(t, cursorType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a) })
	h.Configure(enginetest.PerContact(false), enginetest.Dest)
	call := runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "rx"}}
	status := func() runtime.ReceiverStatus {
		t.Helper()
		st, err := h.RT.ReceiverStatus(h.Ctx, call, h.Channel.Ref)
		if err != nil {
			t.Fatalf("ReceiverStatus: %v", err)
		}
		return st
	}
	if st := status(); st.Owner != "" || !st.Until.IsZero() {
		t.Fatalf("status before Receive = %+v, want no owner", st)
	}
	receive := func(opts runtime.ReceiveOptions) (context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(h.Ctx)
		done := make(chan error, 1)
		go func() { done <- h.RT.Receive(ctx, call, h.Channel.Ref, opts) }()
		select {
		case <-a.runs:
		case err := <-done:
			t.Fatalf("Receive returned %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("receiver did not run")
		}
		return cancel, done
	}
	cancel, done := receive(runtime.ReceiveOptions{Owner: "host-a"})
	if st := status(); st.Owner != "host-a" || !st.Until.Equal(h.Clock.Now().Add(time.Minute)) {
		t.Fatalf("status while receiving = %+v, want host-a until %v", st, h.Clock.Now().Add(time.Minute))
	}
	cancel()
	if err := <-done; err != nil {
		t.Logf("Receive: %v", err)
	}
	if st := status(); st.Owner != "" {
		t.Fatalf("status after Receive returned = %+v, want no owner", st)
	}
	cancel, done = receive(runtime.ReceiveOptions{})
	if st := status(); !strings.HasPrefix(st.Owner, "receiver/") {
		t.Fatalf("status of an unnamed Receive = %+v, want a generated receiver owner", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Logf("Receive: %v", err)
	}

	err := h.RT.Receive(h.Ctx, call, h.Channel.Ref, runtime.ReceiveOptions{Owner: " padded"})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("Receive with a malformed owner = %v, want validation", err)
	}
	other := h.Channel.Ref
	other.ID = h.ID()
	if _, err := h.RT.ReceiverStatus(h.Ctx, call, other); !model.IsCode(err, model.NotFound) {
		t.Fatalf("ReceiverStatus of an unknown channel = %v, want not found", err)
	}
	foreign := h.Channel.Ref
	foreign.Tenant = enginetest.TenantB
	if _, err := h.RT.ReceiverStatus(h.Ctx, call, foreign); !model.IsCode(err, model.NotFound) {
		t.Fatalf("ReceiverStatus of another tenant's channel = %v, want not found", err)
	}
}
