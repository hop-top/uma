package discord

import (
	"context"
	"net/http"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestRouteOf(t *testing.T) {
	for _, c := range []struct {
		method, path, key, major string
		exempt                   bool
	}{
		{"POST", "channels/1/messages", "POST channels/{major}/messages", "channels/1", false},
		{"PATCH", "channels/1/messages/1200000000000000001", "PATCH channels/{major}/messages/{id}", "channels/1", false},
		{"PUT", "channels/1/messages/1200000000000000001/reactions/%F0%9F%91%8D/@me", "PUT channels/{major}/messages/{id}/reactions/{emoji}/@me", "channels/1", false},
		{"POST", "webhooks/1100000000000000001/tok", "POST webhooks/{major}/{token}", "webhooks/1100000000000000001/tok", true},
		{"POST", "interactions/1500000000000000001/tok/callback", "POST interactions/{major}/{token}/callback", "interactions/1500000000000000001", true},
		{"GET", "gateway/bot", "GET gateway/bot", "", false},
	} {
		r := routeOf(c.method, c.path)
		if r.key != c.key || r.major != c.major || r.exempt != c.exempt {
			t.Errorf("routeOf(%s %s) = %+v", c.method, c.path, r)
		}
	}
}

// limiterRig is a limiter on a clock the test moves; sleeping moves it.
type limiterRig struct {
	now   time.Time
	slept []time.Duration
	l     *limiter
}

func newLimiterRig() *limiterRig {
	lr := &limiterRig{now: testNow}
	lr.l = newLimiter(func() time.Time { return lr.now }, func(_ context.Context, d time.Duration) error {
		lr.slept = append(lr.slept, d)
		lr.now = lr.now.Add(d)
		return nil
	})
	return lr
}

func TestLimiterBuckets(t *testing.T) {
	lr := newLimiterRig()
	post := routeOf("POST", "channels/1/messages")
	other := routeOf("POST", "channels/2/messages")
	ctx := t.Context()
	h := http.Header{"X-Ratelimit-Bucket": {"abc"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {"0.5"}}
	if err := lr.l.acquire(ctx, post); err != nil {
		t.Fatal(err)
	}
	lr.l.update(post, h, 200, nil)
	if err := lr.l.acquire(ctx, other); err != nil || len(lr.slept) != 0 {
		t.Fatalf("another channel waited: %v %v", err, lr.slept)
	}
	if err := lr.l.acquire(ctx, post); err != nil || len(lr.slept) != 1 || lr.slept[0] != 500*time.Millisecond {
		t.Fatalf("exhausted bucket: %v slept %v, want 500ms", err, lr.slept)
	}
	h = http.Header{"X-Ratelimit-Bucket": {"abc"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {"30"}}
	lr.l.update(post, h, 200, nil)
	if err := lr.l.acquire(ctx, post); !model.IsCode(err, model.RateLimited) || err.(*model.Error).RetryAfter != 30*time.Second {
		t.Fatalf("long wait = %v, want rate_limited without waiting", err)
	}
}

func TestLimiter429(t *testing.T) {
	lr := newLimiterRig()
	ctx := t.Context()
	post := routeOf("POST", "channels/1/messages")
	lr.l.update(post, http.Header{"X-Ratelimit-Scope": {"user"}}, 429, []byte(`{"retry_after":2.5,"global":false}`))
	if err := lr.l.acquire(ctx, post); !model.IsCode(err, model.RateLimited) || err.(*model.Error).RetryAfter != 2500*time.Millisecond {
		t.Fatalf("after a route 429 = %v", err)
	}
	if err := lr.l.acquire(ctx, routeOf("POST", "channels/2/messages")); err != nil {
		t.Fatalf("another channel after a route 429 = %v", err)
	}
	lr.l.update(post, http.Header{"X-Ratelimit-Global": {"true"}}, 429, []byte(`{"retry_after":3,"global":true}`))
	if err := lr.l.acquire(ctx, routeOf("GET", "channels/3/messages")); !model.IsCode(err, model.RateLimited) {
		t.Fatalf("after a global 429 = %v", err)
	}
	if err := lr.l.acquire(ctx, routeOf("POST", "webhooks/1/tok")); err != nil {
		t.Fatalf("interaction webhook after a global 429 = %v: exempt", err)
	}
}

func TestLimiterGlobalPerSecond(t *testing.T) {
	lr := newLimiterRig()
	for i := range globalPerSecond {
		if err := lr.l.acquire(t.Context(), routeOf("GET", "channels/"+strconvU(uint64(i+1))+"/messages")); err != nil {
			t.Fatal(err)
		}
	}
	if len(lr.slept) != 0 {
		t.Fatalf("slept %v within the limit", lr.slept)
	}
	if err := lr.l.acquire(t.Context(), routeOf("GET", "channels/99/messages")); err != nil || len(lr.slept) != 1 || lr.slept[0] != time.Second {
		t.Fatalf("51st request: %v slept %v, want to the next second", err, lr.slept)
	}
}

// Past the invalid-request ceiling every request is refused until the
// invalid answers age out of ten minutes; a shared-scope 429 is not
// counted.
func TestLimiterInvalidCeiling(t *testing.T) {
	lr := newLimiterRig()
	r := routeOf("GET", "channels/1/messages")
	for range invalidCeiling - 1 {
		lr.l.update(r, nil, http.StatusForbidden, nil)
	}
	lr.l.update(r, http.Header{"X-Ratelimit-Scope": {"shared"}}, 429, []byte(`{"retry_after":0.001}`))
	if err := lr.l.acquire(t.Context(), routeOf("GET", "channels/2/messages")); err != nil {
		t.Fatalf("below the ceiling = %v", err)
	}
	lr.l.update(r, nil, http.StatusUnauthorized, nil)
	if err := lr.l.acquire(t.Context(), routeOf("GET", "channels/2/messages")); !model.IsCode(err, model.RateLimited) {
		t.Fatalf("at the ceiling = %v", err)
	}
	lr.now = lr.now.Add(invalidWindow + time.Minute)
	if err := lr.l.acquire(t.Context(), routeOf("GET", "channels/2/messages")); err != nil {
		t.Fatalf("after ten minutes = %v", err)
	}
}

// Against the stand-in API: a bucket the headers exhaust is waited out
// before the next request, never answered 429.
func TestLimiterWithAPI(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	var slept []time.Duration
	r.x.wait = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		r.clock.t = r.clock.t.Add(d)
		return nil
	}
	r.f.respond("acct1", answer{apply: true, header: map[string]string{"X-RateLimit-Bucket": "b1", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "0.25"}})
	for i := range 2 {
		out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testRoom)), Content: text("m" + strconvU(uint64(i)))})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("send %d = %+v, %v", i, out, err)
		}
	}
	if len(slept) != 1 || slept[0] != 250*time.Millisecond {
		t.Fatalf("slept %v, want the bucket's 250ms before the second send", slept)
	}
}
