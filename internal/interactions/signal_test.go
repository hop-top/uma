package interactions_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

func (r *rig) signal(ttl time.Duration) model.Signal {
	return model.Signal{
		Call: model.Call{Correlation: "corr"}, Conversation: r.conv.Ref, Kind: "typing",
		Audience: public(), ExpiresAt: r.clock.Now().Add(ttl),
	}
}

// A signal reaches the provider once with its audience and leaves nothing
// behind: no operation, event, message or work to replay.
func TestSignalIsSentOnceAndNotRecorded(t *testing.T) {
	r := newRig(t, everything...)
	if err := r.s.Signal(r.ctx, r.signal(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	calls := r.f.recorded()
	if len(calls) != 1 || calls[0].method != "signal" || calls[0].kind != "typing" || calls[0].audience != public() {
		t.Fatalf("provider calls = %+v, want one typing signal to the public audience", calls)
	}
	msgs, evs := r.recorded()
	if len(msgs) != 0 || len(evs) != 0 {
		t.Fatalf("signal recorded messages %v and events %v", msgs, evs)
	}
	if works := r.claim(); len(works) != 0 {
		t.Fatalf("signal left work %+v", works)
	}
	r.drain()
	if n := len(r.f.recorded()); n != 1 {
		t.Fatalf("signal replayed: %v", r.f.methods())
	}
}

// Without the signal capability nothing reaches the adapter.
func TestSignalNeedsCapability(t *testing.T) {
	r := newRig(t, model.CapabilitySend, model.CapabilityEdit, model.CapabilityStream, model.CapabilitySendEphemeral)
	err := r.s.Signal(r.ctx, r.signal(5*time.Second))
	enginetest.RequireCode(t, err, model.Unsupported, "signal")
	if n := len(r.f.recorded()); n != 0 {
		t.Fatalf("provider calls = %v, want none", r.f.methods())
	}
}

// An expired signal is never sent, and one the provider does not take
// before it expires fails with deadline_expired.
func TestSignalExpires(t *testing.T) {
	r := newRig(t, everything...)
	err := r.s.Signal(r.ctx, r.signal(0))
	enginetest.RequireCode(t, err, model.DeadlineExpired, "deadline")
	r.f.block = make(chan struct{})
	err = r.s.Signal(r.ctx, r.signal(20*time.Millisecond))
	enginetest.RequireCode(t, err, model.DeadlineExpired, "deadline")
	if n := len(r.f.recorded()); n != 0 {
		t.Fatalf("provider calls = %v, want none", r.f.methods())
	}
}

// A signal never reads or writes interaction state: with interactions
// stored in the conversation, signalling leaves them, their tokens and the
// policy's interaction actions untouched.
func TestSignalNeverTouchesInteractionState(t *testing.T) {
	r := newRig(t, respondCaps(model.CapabilitySignal)...)
	in := r.seed()
	if err := r.s.Signal(r.ctx, r.signal(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := r.stored(in.Ref)
	if got.Revision != in.Revision || got.Response.Initial != nil || got.Response.FollowUps != 0 {
		t.Fatalf("interaction after a signal = %+v, want untouched", got)
	}
	if r.spy.TokenReads() != 0 || r.spy.Claims() != 0 {
		t.Fatalf("token reads %d, claims %d; want neither", r.spy.TokenReads(), r.spy.Claims())
	}
	for _, action := range []string{model.ActionInteractionRead, model.ActionInteractionRespond} {
		if asks := r.policy.asked(action); len(asks) != 0 {
			t.Fatalf("signal authorized %s: %+v", action, asks)
		}
	}
	if _, evs := r.recorded(); len(evs) != 0 {
		t.Fatalf("signal emitted events %+v", evs)
	}
}
