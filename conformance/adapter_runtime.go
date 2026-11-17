package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// idleWaiter reports each idle Runtime.Run wait and blocks it until the
// test resumes it or the run is cancelled, so the test controls every
// claim round against the fixed clock.
type idleWaiter struct {
	idle   chan struct{}
	resume chan struct{}
}

// discard drops the idle reports no one has read.
func (w *idleWaiter) discard() {
	for {
		select {
		case <-w.idle:
		default:
			return
		}
	}
}

func (w *idleWaiter) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case w.idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.resume:
		return nil
	}
}

// reply queues a public reply on conv under operation opID.
func (e *adapterEnv) reply(conv model.ConversationRef, opID model.OperationID) (model.Acceptance, error) {
	return e.client.Messages().Reply(e.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(opID))}, Operation: opID},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "reply " + string(opID)}}},
	})
}

func (e *adapterEnv) mustReply(t *testing.T, conv model.ConversationRef, opID model.OperationID) model.OperationRef {
	t.Helper()
	a, err := e.reply(conv, opID)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if a.Operation.State != model.Queued {
		t.Fatalf("Reply state = %q, want queued", a.Operation.State)
	}
	return a.Operation.Ref
}

// start runs a worker in the background until it first goes idle.
func (e *adapterEnv) start(t *testing.T) {
	t.Helper()
	e.run = make(chan error, 1)
	go func() {
		e.run <- e.engine.Runtime().Run(e.ctx, runtimeCall(), uma.WorkerOptions{Owner: "conformance-worker", Limit: 10})
	}()
	t.Cleanup(func() {
		if e.run != nil {
			_ = e.engine.Runtime().Drain(context.Background(), runtimeCall())
			<-e.run
		}
	})
	e.idle(t)
}

// idle waits until the worker has no claimable work.
func (e *adapterEnv) idle(t *testing.T) {
	t.Helper()
	select {
	case <-e.waiter.idle:
	case err := <-e.run:
		e.run = nil
		t.Fatalf("Runtime.Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Runtime.Run did not go idle")
	}
}

// later advances the clock by d and lets the idle worker claim again.
func (e *adapterEnv) later(t *testing.T, d time.Duration) {
	t.Helper()
	e.clock.Advance(d)
	e.waiter.resume <- struct{}{}
	e.idle(t)
}

// stop drains the worker and checks it ended cleanly.
func (e *adapterEnv) stop(t *testing.T) {
	t.Helper()
	if err := e.engine.Runtime().Drain(e.ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	err := <-e.run
	e.run = nil
	if err != nil {
		t.Fatalf("Runtime.Run: %v", err)
	}
}

func (e *adapterEnv) op(t *testing.T, ref model.OperationRef) model.Operation {
	t.Helper()
	op, err := e.client.Operations().Get(e.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: ref})
	if err != nil {
		t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// receive runs Runtime.Receive for ch in the background until the case
// ends or the returned stop is called. The run's own result is not judged:
// cases observe the provider and the store.
func (e *adapterEnv) receive(t *testing.T, ch model.ChannelRef) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.engine.Runtime().Receive(ctx, runtimeCall(), ch)
	}()
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
			// The receiver's idle waits are no worker's.
			e.waiter.discard()
		}
	}
	t.Cleanup(stop)
	return stop
}

// awaitInbound waits until the adapter has handed every event of evs on
// ch's account to a sink after the first before results, and returns
// those results, one per event in evs order: the first one naming it. A
// count of results is not enough: an adapter may legally hand an earlier
// event on again (at-least-once, such as a receiver re-reading after a
// stop that interrupted its cursor save), and that redelivery is no
// result of these events.
func (e *adapterEnv) awaitInbound(t *testing.T, ch model.ChannelRef, before int, evs []InboundEvent) []InboundResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := e.provider.Inbound(ch)
		out := make([]InboundResult, 0, len(evs))
		for _, ev := range evs {
			for _, r := range got[min(before, len(got)):] {
				if r.EventID == ev.EventID {
					out = append(out, r)
					break
				}
			}
		}
		if len(out) == len(evs) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("adapter handed %d of %d inbound events of channel %s to a sink", len(out), len(evs), ch.ID)
		}
		time.Sleep(time.Millisecond)
	}
}

// receiveAll delivers evs to ch's account, hands them to the engine with
// pump and returns their results.
func (e *adapterEnv) receiveAll(t *testing.T, ch model.ChannelRef, evs ...InboundEvent) []InboundResult {
	t.Helper()
	before := len(e.provider.Inbound(ch))
	for _, ev := range evs {
		e.deliver(t, ch, ev)
	}
	return e.pump(t, ch, before, evs...)
}

// pump hands the events queued on ch's account to the engine until each
// of evs was handed to a sink after the first before results, and returns
// their results: through Runtime.Receive, run until then and stopped, for
// an adapter that receives, else by pushing them to the handler of
// Runtime.Webhook.
func (e *adapterEnv) pump(t *testing.T, ch model.ChannelRef, before int, evs ...InboundEvent) []InboundResult {
	t.Helper()
	if !slices.Contains(e.desc.Capabilities, model.CapabilityReceive) {
		h, err := e.engine.Runtime().Webhook(e.ctx, runtimeCall(), ch)
		if err != nil {
			t.Fatalf("Runtime.Webhook: %v", err)
		}
		if err := e.provider.(WebhookProvider).Push(ch, h); err != nil {
			t.Fatalf("Push: %v", err)
		}
		return e.awaitInbound(t, ch, before, evs)
	}
	stop := e.receive(t, ch)
	got := e.awaitInbound(t, ch, before, evs)
	stop()
	return got
}

// requireInbound skips an inbound case the adapter and provider cannot run:
// one that neither receives nor receives over HTTP with a WebhookProvider.
func (e *adapterEnv) requireInbound(t *testing.T) {
	t.Helper()
	caps := e.desc.Capabilities
	switch {
	case slices.Contains(caps, model.CapabilityReceive):
	case !slices.Contains(caps, model.CapabilityReceiveHTTP):
		t.Skip("capabilities receive and receive_http not declared")
	default:
		if _, ok := e.provider.(WebhookProvider); !ok {
			t.Skip("receive_http provider does not implement WebhookProvider")
		}
	}
}
