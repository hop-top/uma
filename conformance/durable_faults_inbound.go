package conformance

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Ingress and subscription fault cases.

// An ingress unit dies before it commits, with its receiver: nothing was
// accepted, so the provider redelivers. The crashed receiver's lease keeps
// the channel until it expires; then the next process's receiver takes it
// over and accepts the event once.
func ingressCrashBeforeCommit(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	p := f.start(t)
	p.store.at(beforeCommit, (*unitLog).claimsIngress, p.store.crash)
	done, _ := p.receive()
	in := f.message("evt-1", "m-1")
	f.push(t, in)
	if r := f.accept(t); r.err == nil {
		t.Fatalf("ingress = %+v, accepted by a unit that crashed before commit", r.receipt)
	}
	wait(t, done)
	p = f.restart(t, cross)
	if n := len(f.messages(t)); n != 0 {
		t.Fatalf("messages after the crash = %d, want none", n)
	}
	wantCode(t, p.engine.Runtime().Receive(p.ctx, faultCall(), f.ch), model.Conflict)
	e.fx.Advance(leaseExpiry)
	_, stop := p.receive()
	f.push(t, in)
	if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
		t.Fatalf("redelivery = %+v, %v; want it committed once", r.receipt, r.err)
	}
	f.push(t, in)
	if r := f.accept(t); r.err != nil || !r.receipt.Duplicate {
		t.Fatalf("second redelivery = %+v, %v; want a duplicate", r.receipt, r.err)
	}
	stop()
	f.wantOneMessage(t)
}

// A provider delivers one event twice at once: the second delivery claims
// it while the first one's unit is still uncommitted. Exactly one unit
// records the message; the other reports a duplicate.
func duplicateDuringIngressCommit(t *testing.T, e *env, _ func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	p := f.start(t)
	committing := f.gate("first delivery commit")
	p.store.at(beforeCommit, (*unitLog).claimsIngress, func() error {
		committing.reach()
		return nil
	})
	var claims atomic.Int32
	second := f.gate("second delivery")
	second.open()
	p.store.onIngressClaim(func(store.IngressRecord) {
		if claims.Add(1) == 2 {
			second.reach()
		}
	})
	_, stop := p.receive()
	in := f.message("evt-1", "m-1")
	f.push(t, in)
	committing.await(t)
	f.push(t, in)
	second.await(t)
	committing.open()
	first, other := f.accept(t), f.accept(t)
	stop()
	if first.err != nil || other.err != nil {
		t.Fatalf("deliveries = %v, %v; want both acknowledged", first.err, other.err)
	}
	if first.receipt.Duplicate == other.receipt.Duplicate || first.receipt.ID != other.receipt.ID {
		t.Fatalf("receipts = %+v and %+v, want one commit and one duplicate of it", first.receipt, other.receipt)
	}
	f.wantOneMessage(t)
}

// A consumer polls a batch and its process restarts before the ack. The
// batch and its token survive: the next process sees the same outstanding
// batch, and once its lease expires the batch is redelivered under a new
// token while the old one is fenced.
func batchSurvivesRestart(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	p := f.start(t)
	p.reply(t, "op-1")
	sub := p.open(t)
	b := p.poll(t, sub)
	if len(b.Events) == 0 || b.AckToken == "" {
		t.Fatalf("batch = %+v, want the committed events under a token", b)
	}
	p = f.restart(t, cross)
	again := p.poll(t, sub)
	if again.AckToken != b.AckToken || !slices.Equal(eventIDs(again.Events), eventIDs(b.Events)) {
		t.Fatalf("batch after restart = %+v, want the outstanding one %+v", again, b)
	}
	e.fx.Advance(b.ExpiresAt.Sub(e.fx.Now()) + time.Second)
	_, err := p.ack(sub, "ack-stale", b.AckToken)
	wantCode(t, err, model.Conflict)
	redelivered := p.poll(t, sub)
	if redelivered.AckToken == b.AckToken || !slices.Equal(eventIDs(redelivered.Events), eventIDs(b.Events)) {
		t.Fatalf("redelivery = %+v, want the same events under a new token", redelivered)
	}
	r, err := p.ack(sub, "ack-1", redelivered.AckToken)
	if err != nil || r.Subscription.Checkpoint != redelivered.ScannedThrough {
		t.Fatalf("ack = %+v, %v; want the checkpoint at the batch end", r, err)
	}
	if rest := p.poll(t, sub); len(rest.Events) != 0 {
		t.Fatalf("after the ack poll = %d events, want none", len(rest.Events))
	}
}

// The ack unit dies before it commits. The checkpoint did not move; the
// next process acknowledges the same batch with the same operation and
// moves it once.
func ackCrashBeforeCommit(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	p := f.start(t)
	p.reply(t, "op-1")
	sub := p.open(t)
	b := p.poll(t, sub)
	p.store.at(beforeCommit, (*unitLog).acks, p.store.crash)
	if _, err := p.ack(sub, "ack-1", b.AckToken); err == nil {
		t.Fatal("ack committed by a unit that crashed before commit")
	}
	p = f.restart(t, cross)
	r, err := p.ack(sub, "ack-1", b.AckToken)
	if err != nil || r.Acceptance.Replayed || r.Subscription.Checkpoint != b.ScannedThrough {
		t.Fatalf("ack after restart = %+v, %v; want a first ack moving the checkpoint", r, err)
	}
	if rest := p.poll(t, sub); len(rest.Events) != 0 {
		t.Fatalf("after the ack poll = %d events, want none", len(rest.Events))
	}
}

// The ack commits and the process dies before answering. Retrying the same
// ack operation after the restart replays the stored result: the
// checkpoint moved once and nothing is redelivered.
func ackLostAfterCommit(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	p := f.start(t)
	p.reply(t, "op-1")
	sub := p.open(t)
	b := p.poll(t, sub)
	p.store.at(afterCommit, (*unitLog).acks, p.store.crash)
	if _, err := p.ack(sub, "ack-1", b.AckToken); err == nil {
		t.Fatal("ack answered by a process that crashed before answering")
	}
	p = f.restart(t, cross)
	r, err := p.ack(sub, "ack-1", b.AckToken)
	if err != nil || !r.Acceptance.Replayed || r.Subscription.Checkpoint != b.ScannedThrough {
		t.Fatalf("retried ack = %+v, %v; want the stored result replayed", r, err)
	}
	if rest := p.poll(t, sub); len(rest.Events) != 0 {
		t.Fatalf("after the ack poll = %d events, want none", len(rest.Events))
	}
}

// message is a verified inbound message from one sender to the inbox.
func (f *faultEnv) message(event, msg string) model.Ingress {
	return f.messageFrom(event, msg, "u-1")
}

// messageFrom is a verified inbound message from sender to the inbox.
func (f *faultEnv) messageFrom(event, msg, sender string) model.Ingress {
	return model.Ingress{Call: model.Call{Correlation: "rx-" + model.CorrelationID(event)}, Envelope: model.Envelope{
		EventID: event, MessageID: msg, Destination: faultDest, Kind: model.EnvelopeMessage,
		Sender:    model.ExternalRef{Channel: f.ch, Namespace: faultNS, Kind: "user", ID: sender},
		Assurance: "verified",
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello " + event}}},
		Audience:  model.Audience{Visibility: model.Public}, OccurredAt: f.fx.Now(),
	}}
}

// push delivers in to the running receiver.
func (f *faultEnv) push(t *testing.T, in model.Ingress) {
	t.Helper()
	select {
	case f.p.inbound <- in:
	case <-time.After(10 * time.Second):
		t.Fatal("no receiver took the delivery")
	}
}

// accept waits for the receiver's next sink result.
func (f *faultEnv) accept(t *testing.T) accepted {
	t.Helper()
	select {
	case r := <-f.p.accepted:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("receiver handed nothing to its sink")
		return accepted{}
	}
}

func (f *faultEnv) messages(t *testing.T) []model.Message {
	t.Helper()
	var out []model.Message
	f.mustView(t, func(r store.Reader) error {
		p, err := r.Messages(f.ctx, list(100))
		out = p.Items
		return err
	})
	return out
}

// wantOneMessage checks the inbox recorded one message and one
// message.received event.
func (f *faultEnv) wantOneMessage(t *testing.T) {
	t.Helper()
	if n := len(f.messages(t)); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
	var received int
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind == model.EventMessageReceived {
			received++
		}
	}
	if received != 1 {
		t.Fatalf("message.received events = %d, want 1", received)
	}
}

// receive runs the channel's receiver in the background until stop is
// called or the process ends.
func (p *faultProc) receive() (done <-chan error, stop func()) {
	ctx, cancel := context.WithCancel(p.ctx)
	ch := make(chan error, 1)
	go func() { ch <- p.engine.Runtime().Receive(ctx, faultCall(), p.f.ch) }()
	return ch, func() {
		cancel()
		<-ch
	}
}

func wait(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not stop")
	}
}

func (p *faultProc) open(t *testing.T) model.SubscriptionRef {
	t.Helper()
	r, err := p.client.Events().Open(p.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "open"}, Operation: "open-1"},
		Consumer: "bot", Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		t.Fatalf("Events.Open: %v", err)
	}
	return r.Subscription.Ref
}

func (p *faultProc) poll(t *testing.T, sub model.SubscriptionRef) model.EventBatch {
	t.Helper()
	b, err := p.client.Events().Poll(p.ctx, model.Poll{Call: model.Call{Correlation: "poll"}, Subscription: sub, Limit: 10})
	if err != nil {
		t.Fatalf("Events.Poll: %v", err)
	}
	return b
}

func (p *faultProc) ack(sub model.SubscriptionRef, opID model.OperationID, token string) (model.SubscriptionResult, error) {
	return p.client.Events().Ack(p.ctx, model.Acknowledge{
		Command: model.Command{Call: model.Call{Correlation: "ack"}, Operation: opID}, Subscription: sub, Token: token,
	})
}
