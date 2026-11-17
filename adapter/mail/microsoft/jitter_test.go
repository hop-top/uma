package microsoft

import (
	"testing"
	"time"
)

// jitterAt makes the factory's jitter source answer u, in [0, 1].
func jitterAt(e *env, u float64) { e.inst.f.opts.Jitter = func() float64 { return u } }

// The poll wait is the poll interval +-10%, following the injected source.
func TestPollWaitIsSpread(t *testing.T) {
	const poll = 5 * time.Minute
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, poll * 9 / 10}, {0.5, poll}, {1, poll * 11 / 10}} {
		e := newEnv(t, Config{})
		jitterAt(e, c.u)
		wait, err := e.inst.round(t.Context(), &testSink{})
		if err != nil {
			t.Fatal(err)
		}
		if wait != c.want {
			t.Errorf("source %v: wait %v, want %v", c.u, wait, c.want)
		}
	}
}

// With no injected source every wait stays within +-10% and the waits are
// not all equal, so channels drift out of phase.
func TestPollWaitDefaultSourceVaries(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.f.opts.Jitter = nil
	seen := map[time.Duration]bool{}
	for range 50 {
		wait, err := e.inst.round(t.Context(), &testSink{})
		if err != nil {
			t.Fatal(err)
		}
		if wait < 5*time.Minute*9/10 || wait > 5*time.Minute*11/10 {
			t.Fatalf("wait %v outside +-10%% of the poll interval", wait)
		}
		seen[wait] = true
	}
	if len(seen) < 10 {
		t.Fatalf("50 waits took only %d values", len(seen))
	}
}

// The renewal time is the margin before expiry +-10%: drawn once when the
// subscription is made, kept until the next renewal, and the subscription
// is renewed there, not at the plain margin.
func TestSubscriptionRenewalIsSpread(t *testing.T) {
	margin := renewMargin(defaultSubscriptionLifetime)
	for _, c := range []struct {
		u      float64
		spread time.Duration
	}{{0, margin * 9 / 10}, {1, margin * 11 / 10}} {
		e := newEnv(t, Config{NotificationURL: hookURL})
		jitterAt(e, c.u)
		if err := round(t, e, &testSink{}); err != nil {
			t.Fatal(err)
		}
		exp := onlySub(t, e).ExpirationDateTime
		st, err := e.inst.loadState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := exp.Add(-c.spread); !st.SubscriptionRenewAt.Equal(want) {
			t.Fatalf("source %v: renew at %v, want %v", c.u, st.SubscriptionRenewAt, want)
		}
		// Not renewed a minute before it is due, renewed once due.
		e.clock.Advance(st.SubscriptionRenewAt.Sub(e.clock.Now()) - time.Minute)
		if err := round(t, e, &testSink{}); err != nil {
			t.Fatal(err)
		}
		if n := e.g.count("PATCH /v1.0/subscriptions/"); n != 0 {
			t.Fatalf("source %v: renewed %d times before due", c.u, n)
		}
		e.clock.Advance(time.Minute)
		if err := round(t, e, &testSink{}); err != nil {
			t.Fatal(err)
		}
		if n := e.g.count("PATCH /v1.0/subscriptions/"); n != 1 {
			t.Fatalf("source %v: renewed %d times once due, want 1", c.u, n)
		}
		// Both ends renew well before the subscription expires.
		if !st.SubscriptionRenewAt.Before(exp.Add(-margin * 9 / 10 / 2)) {
			t.Fatalf("source %v: renewal %v too close to expiry %v", c.u, st.SubscriptionRenewAt, exp)
		}
	}
}

// A state saved before the renewal time was kept renews at the plain margin.
func TestSubscriptionRenewalWithoutStoredTime(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: hookURL})
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	st, err := e.inst.loadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	st.SubscriptionRenewAt = time.Time{}
	if err := e.inst.saveState(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	margin := renewMargin(defaultSubscriptionLifetime)
	e.clock.Advance(st.SubscriptionExpires.Add(-margin).Sub(e.clock.Now()))
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("PATCH /v1.0/subscriptions/"); n != 1 {
		t.Fatalf("renewed %d times at the plain margin, want 1", n)
	}
}
