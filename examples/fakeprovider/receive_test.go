package fakeprovider_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// flaky wraps a fake provider factory: the first fail receiver runs of its
// instances return the scripted error before delivering anything.
type flaky struct {
	*fakeprovider.Factory
	mu   sync.Mutex
	fail int
	err  error
	runs int
}

func (f *flaky) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return &flakyInstance{Instance: inst, f: f}, nil
}

type flakyInstance struct {
	adapter.Instance
	f *flaky
}

func (i *flakyInstance) Run(ctx context.Context, s adapter.Sink) error {
	i.f.mu.Lock()
	i.f.runs++
	fail := i.f.fail > 0
	if fail {
		i.f.fail--
	}
	i.f.mu.Unlock()
	if fail {
		return i.f.err
	}
	return i.Instance.(adapter.Receiver).Run(ctx, s)
}

func (f *flaky) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func newReceiveHost(t *testing.T, fail int, err error) (*inboundHost, *flaky) {
	f := &flaky{Factory: fakeprovider.NewIngressOnly(), fail: fail, err: err}
	return newInboundHost(t, f, f.Config()), f
}

func inIngress(id, dest string) model.Ingress {
	return model.Ingress{Call: model.Call{Correlation: "rx"}, Envelope: model.Envelope{
		EventID: id, MessageID: "m-" + id, Destination: dest, Kind: model.EnvelopeMessage,
		Sender:    model.ExternalRef{Channel: inChannel, Namespace: fakeprovider.Namespace, Kind: "user", ID: "u1"},
		Assurance: "verified",
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi " + id}}},
		Audience:  model.Audience{Visibility: model.Public}, OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
	}}
}

// receive starts Runtime.Receive in the background.
func (h *inboundHost) receive(ctx context.Context, opts ...uma.ReceiveOption) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.engine.Runtime().Receive(ctx, inCall(), inChannel, opts...) }()
	return done
}

func (h *inboundHost) acked(f *flaky, n int) {
	h.t.Helper()
	inEventually(h.t, "acknowledged deliveries", func() bool {
		k := 0
		for _, d := range f.Script(inChannel).Deliveries() {
			if d.Acked {
				k++
			}
		}
		return k >= n
	})
}

func returned(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Receive did not return")
		return nil
	}
}

func TestReceiveRoutesOneConnectionToSeveralInboxesAndDrains(t *testing.T) {
	h, f := newReceiveHost(t, 0, nil)
	f.Script(inChannel).Push(inIngress("e1", "support"), inIngress("e2", "sales"), inIngress("e1", "support"))
	done := h.receive(h.ctx)
	h.acked(f, 3)
	if err := h.engine.Runtime().Drain(h.ctx, inCall()); err != nil {
		t.Fatal(err)
	}
	if err := returned(t, done); err != nil {
		t.Fatalf("Receive after Drain = %v, want nil", err)
	}
	if s, m := len(h.messages(inSupport)), len(h.messages(inSales)); s != 1 || m != 1 {
		t.Fatalf("support %d, sales %d messages; want one each (duplicate delivery idempotent)", s, m)
	}
	if d := f.Script(inChannel).Deliveries(); !d[2].Receipt.Duplicate {
		t.Fatalf("redelivered event receipt = %+v, want duplicate", d[2].Receipt)
	}
	// Drain stops later receivers for the tenant too.
	inCode(t, h.engine.Runtime().Receive(h.ctx, inCall(), inChannel), model.Unavailable)
}

func TestReceiveSecondOwnerConflicts(t *testing.T) {
	h, f := newReceiveHost(t, 0, nil)
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.receive(ctx, uma.ReceiveOwner("host-a"))
	f.Script(inChannel).Push(inIngress("e1", "support"))
	h.acked(f, 1)
	probe, stop := context.WithTimeout(h.ctx, time.Second)
	defer stop()
	inCode(t, h.engine.Runtime().Receive(probe, inCall(), inChannel, uma.ReceiveOwner("host-b")), model.Conflict)
	// The status names the owner without another Receive conflicting.
	st, err := h.engine.Runtime().ReceiverStatus(h.ctx, inCall(), inChannel)
	if err != nil || st.Owner != "host-a" || st.Until.IsZero() {
		t.Fatalf("ReceiverStatus = %+v, %v; want host-a with an expiry", st, err)
	}
	cancel()
	if err := returned(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive after cancel = %v, want context.Canceled", err)
	}
	// The cancelled owner released its lease: the channel is free again.
	ctx2, cancel2 := context.WithCancel(h.ctx)
	again := h.receive(ctx2)
	f.Script(inChannel).Push(inIngress("e2", "support"))
	h.acked(f, 2)
	cancel2()
	if err := returned(t, again); !errors.Is(err, context.Canceled) {
		t.Fatalf("second owner = %v", err)
	}
}

func TestReceiveReconnectsAfterTransientFailure(t *testing.T) {
	h, f := newReceiveHost(t, 2, model.NewError(model.Transient, "connection reset"))
	f.Script(inChannel).Push(inIngress("e1", "support"))
	done := h.receive(h.ctx)
	h.acked(f, 1)
	if n := h.inPacer.waits(); n != 2 {
		t.Fatalf("backoff waits = %d, want 2 (one per failed connection)", n)
	}
	if err := h.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := returned(t, done); err != nil {
		t.Fatalf("Receive after Close = %v, want nil", err)
	}
	if len(h.messages(inSupport)) != 1 {
		t.Fatal("ingress after reconnect not committed")
	}
}

func TestReceiveGivesUpAfterRetryBudget(t *testing.T) {
	h, f := newReceiveHost(t, 100, errors.New("dial tcp 10.0.0.1:443: provider internals"))
	err := returned(t, h.receive(h.ctx))
	inCode(t, err, model.Unavailable)
	var me *model.Error
	if !errors.As(err, &me) || me.SafeMessage != "receiver failed" {
		t.Fatalf("err = %#v, want the safe receiver failure, no provider detail", err)
	}
	if f.runCount() != 3 || h.inPacer.waits() != 2 {
		t.Fatalf("runs %d, waits %d; want 3 connections and 2 backoffs", f.runCount(), h.inPacer.waits())
	}
}

func TestReceivePermanentFailureReturnsAtOnce(t *testing.T) {
	h, f := newReceiveHost(t, 0, nil)
	f.Script(inChannel).Revoke(model.CapabilityReceive)
	inCode(t, returned(t, h.receive(h.ctx)), model.Unauthorized)
	if h.inPacer.waits() != 0 {
		t.Fatalf("backoff waits = %d after a permanent failure, want 0", h.inPacer.waits())
	}
}

func TestReceiveRefusesChannelsWithoutReceive(t *testing.T) {
	f := fakeprovider.NewWebhook()
	h := newInboundHost(t, f, f.Config())
	inCode(t, h.engine.Runtime().Receive(h.ctx, inCall(), inChannel), model.Unsupported)
}

// An owner whose lease expired and was taken over stops at its next renewal.
func TestReceiveStopsWhenOwnershipIsLost(t *testing.T) {
	h, f := newReceiveHost(t, 0, nil)
	done := h.receive(h.ctx)
	f.Script(inChannel).Push(inIngress("e1", "support"))
	h.acked(f, 1)
	h.clock.Advance(2 * time.Minute)
	h.tx(func(tx store.Tx) error {
		_, err := tx.AcquireLease(h.ctx, store.LeaseRequest{Resource: inChannel.Ref(), Owner: "other-process", Until: h.clock.Now().Add(time.Minute)})
		return err
	})
	h.inPacer.renew <- struct{}{}
	inCode(t, returned(t, done), model.Conflict)
}

// A live owner renews its lease, so a later owner still conflicts long after
// the first lease would have expired. Renewal k lands before the clock
// passes 25s*(k+1), always inside the lease it renews.
func TestReceiveRenewsItsLease(t *testing.T) {
	h, f := newReceiveHost(t, 0, nil)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := h.receive(ctx)
	f.Script(inChannel).Push(inIngress("e1", "support"))
	h.acked(f, 1)
	for i := range 3 {
		h.clock.Advance(25 * time.Second)
		select {
		case h.inPacer.renew <- struct{}{}:
		case err := <-done:
			t.Fatalf("Receive returned before renewal %d: %v", i+1, err)
		}
	}
	probe, stop := context.WithTimeout(h.ctx, time.Second)
	defer stop()
	inCode(t, h.engine.Runtime().Receive(probe, inCall(), inChannel), model.Conflict)
	cancel()
	if err := returned(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v", err)
	}
}
