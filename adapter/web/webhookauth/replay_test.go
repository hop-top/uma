package webhookauth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
)

// tick is a clock tests move.
type tick struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tick) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *tick) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// recordingStore wraps a store and records what the verifier asks of it.
type recordingStore struct {
	webhookauth.ReplayStore
	keys     []string
	ttls     []time.Duration
	released []string
}

func (s *recordingStore) Reserve(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	s.keys, s.ttls = append(s.keys, key), append(s.ttls, ttl)
	return s.ReplayStore.Reserve(ctx, key, ttl)
}

func (s *recordingStore) Release(ctx context.Context, key string) error {
	s.released = append(s.released, key)
	return s.ReplayStore.Release(ctx, key)
}

type brokenStore struct{ err error }

func (s brokenStore) Reserve(context.Context, string, time.Duration) (bool, error) {
	return false, s.err
}
func (s brokenStore) Release(context.Context, string) error { return s.err }

func sigOf(body string) string { return macHex(sha256New, []byte(longSecret), []byte(body)) }

func bySignature(c webhookauth.Config) webhookauth.Config {
	c.Scheme = webhookauth.SchemeHMAC
	c.Replay.Source = webhookauth.ReplayBySignature
	return c
}

func TestReplayBySignature(t *testing.T) {
	store := webhookauth.NewMemoryReplayStore(0, clock{now})
	v := mustNew(t, bySignature(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{Replay: store, Scope: "ch1"})
	h := hdr("X-Webhook-Signature", sigOf("b"))
	if err := verify(t, v, h, "b"); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	wantErr(t, verify(t, v, h, "b"), webhookauth.ErrReplay)
	// A different message is a different delivery.
	if err := verify(t, v, hdr("X-Webhook-Signature", sigOf("c")), "c"); err != nil {
		t.Fatalf("other delivery: %v", err)
	}
	// The same MAC written another way is the same delivery: the key is the
	// decoded signature, so re-encoding a captured request cannot dodge it.
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", strings.ToUpper(sigOf("b"))), "b"), webhookauth.ErrReplay)
	if webhookauth.Status(webhookauth.ErrReplay) != 409 {
		t.Errorf("replay status = %d, want 409", webhookauth.Status(webhookauth.ErrReplay))
	}
}

func TestReplayKeyIsolatedByScope(t *testing.T) {
	store := webhookauth.NewMemoryReplayStore(0, clock{now})
	mk := func(scope string) *webhookauth.Verifier {
		return mustNew(t, bySignature(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{Replay: store, Scope: scope})
	}
	h := hdr("X-Webhook-Signature", sigOf("b"))
	for _, scope := range []string{"a", "b"} {
		if err := verify(t, mk(scope), h, "b"); err != nil {
			t.Fatalf("scope %q: %v", scope, err)
		}
	}
	wantErr(t, verify(t, mk("a"), h, "b"), webhookauth.ErrReplay)
}

// Only an authentic request may take a replay slot: otherwise anyone could
// burn the id of a delivery that has not arrived yet.
func TestReplayReservedOnlyAfterVerification(t *testing.T) {
	store := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
	cfg := webhookauth.Config{
		Scheme: webhookauth.SchemeHMAC, Payload: "{id}.{body}",
		Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Delivery-Id"},
	}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{Replay: store})
	forged := hdr("X-Webhook-Signature", strings.Repeat("00", 32), "X-Delivery-Id", "d1")
	wantErr(t, verify(t, v, forged, "b"), webhookauth.ErrInvalid)
	if len(store.keys) != 0 {
		t.Fatalf("a forged request reserved %d keys", len(store.keys))
	}
	good := hdr("X-Webhook-Signature", sigOf("d1.b"), "X-Delivery-Id", "d1")
	if err := verify(t, v, good, "b"); err != nil {
		t.Fatalf("genuine delivery blocked after a forged one: %v", err)
	}
	wantErr(t, verify(t, v, good, "b"), webhookauth.ErrReplay)
}

// A delivery id the signature does not cover can be swapped by anyone, so
// it is not a defence.
func TestReplayHeaderMustBeSigned(t *testing.T) {
	src := secrets(longSecret)
	store := webhookauth.NewMemoryReplayStore(0, clock{now})
	deps := webhookauth.Deps{Replay: store}
	rp := webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Delivery-Id"}
	_, err := webhookauth.New(webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: rp}, src, deps)
	wantErr(t, err, webhookauth.ErrConfig)
	rp.AllowUnsigned = true
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: rp}, src, deps)
	// The weakness is the reason for the consent: a captured request with a
	// new id passes.
	for _, id := range []string{"1", "2"} {
		if err := verify(t, v, hdr("X-Webhook-Signature", sigOf("b"), "X-Delivery-Id", id), "b"); err != nil {
			t.Fatalf("id %s: %v", id, err)
		}
	}
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sigOf("b"), "X-Delivery-Id", "1"), "b"), webhookauth.ErrReplay)
	// Bearer and token sign nothing: same consent.
	_, err = webhookauth.New(webhookauth.Config{Scheme: webhookauth.SchemeBearer, Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Delivery-Id"}}, src, deps)
	wantErr(t, err, webhookauth.ErrConfig)
}

func TestReplayHeaderProblems(t *testing.T) {
	store := webhookauth.NewMemoryReplayStore(0, clock{now})
	cfg := webhookauth.Config{
		Scheme: webhookauth.SchemeHMAC, Payload: "{id}{body}",
		Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Delivery-Id"},
	}
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{Replay: store})
	sig := sigOf("d1b")
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig), "b"), webhookauth.ErrMissing)
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig, "X-Delivery-Id", ""), "b"), webhookauth.ErrMissing)
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig, "X-Delivery-Id", "d1", "X-Delivery-Id", "d1"), "b"), webhookauth.ErrMalformed)
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig, "X-Delivery-Id", strings.Repeat("d", 300)), "b"), webhookauth.ErrMalformed)
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", sig, "X-Delivery-Id", "d\x001"), "b"), webhookauth.ErrMalformed)
}

func TestReplayRelease(t *testing.T) {
	store := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
	v := mustNew(t, bySignature(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{Replay: store})
	req := webhookauth.Request{Header: hdr("X-Webhook-Signature", sigOf("b")), Body: []byte("b")}
	ctx := context.Background()
	first, err := v.Verify(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// The host failed to take the event and tells the sender to retry: the
	// retry must not be mistaken for a replay.
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	retry, err := v.Verify(ctx, req)
	if err != nil {
		t.Fatalf("retry after release: %v", err)
	}
	// Releasing twice never frees the retry's reservation.
	_ = first.Release(ctx)
	if len(store.released) != 1 {
		t.Fatalf("store released %d times, want once", len(store.released))
	}
	wantErr(t, verify(t, v, req.Header, "b"), webhookauth.ErrReplay)
	_ = retry
	// A result with nothing reserved releases nothing.
	plain := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	res, err := plain.Verify(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Release(ctx); err != nil {
		t.Fatalf("Release on a result without replay: %v", err)
	}
	var none *webhookauth.Result
	if err := none.Release(ctx); err != nil {
		t.Fatalf("Release on nil: %v", err)
	}
}

func TestReplayStoreFailureFailsClosed(t *testing.T) {
	down := errors.New("redis down")
	v := mustNew(t, bySignature(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{Replay: brokenStore{down}})
	err := verify(t, v, hdr("X-Webhook-Signature", sigOf("b")), "b")
	wantErr(t, err, webhookauth.ErrUnavailable)
	wantErr(t, err, down)
	if webhookauth.Status(err) != 503 {
		t.Errorf("status = %d, want 503", webhookauth.Status(err))
	}
}

func TestReplayConfigErrors(t *testing.T) {
	src := secrets(longSecret)
	store := webhookauth.NewMemoryReplayStore(0, nil)
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
		deps webhookauth.Deps
	}{
		{"replay without a store", bySignature(webhookauth.Config{}), webhookauth.Deps{}},
		{"unknown source", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: webhookauth.Replay{Source: "nonce"}}, webhookauth.Deps{Replay: store}},
		{"signature source on bearer", webhookauth.Config{Scheme: webhookauth.SchemeBearer, Replay: webhookauth.Replay{Source: webhookauth.ReplayBySignature}}, webhookauth.Deps{Replay: store}},
		{"signature source on token", webhookauth.Config{Scheme: webhookauth.SchemeToken, Replay: webhookauth.Replay{Source: webhookauth.ReplayBySignature}}, webhookauth.Deps{Replay: store}},
		{"signature source with a header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: webhookauth.Replay{Source: webhookauth.ReplayBySignature, Header: "X-Id"}}, webhookauth.Deps{Replay: store}},
		{"header source without a header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, AllowUnsigned: true}}, webhookauth.Deps{Replay: store}},
		{"header source with a bad header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X Id", AllowUnsigned: true}}, webhookauth.Deps{Replay: store}},
		{"ttl without a source", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: webhookauth.Replay{TTL: time.Hour}}, webhookauth.Deps{Replay: store}},
		{"negative ttl", bySignature(webhookauth.Config{Replay: webhookauth.Replay{TTL: -time.Hour}}), webhookauth.Deps{Replay: store}},
		{"id placeholder without header source", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Payload: "{id}{body}"}, webhookauth.Deps{Replay: store}},
		{"id header is the credential header", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "x-webhook-signature", AllowUnsigned: true}}, webhookauth.Deps{Replay: store}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhookauth.New(tc.cfg, src, tc.deps)
			wantErr(t, err, webhookauth.ErrConfig)
		})
	}
}

// An entry must outlive the window in which its request still looks fresh.
// A request stamped at the far edge of the future tolerance stays fresh for
// two tolerances, so the entry lives that long, not one.
func TestReplayEntryOutlivesTimestampWindow(t *testing.T) {
	clk := &tick{t: now}
	store := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clk)}
	cfg := bySignature(webhookauth.Config{Timestamp: webhookauth.Timestamp{Header: "X-T", Tolerance: time.Minute}})
	v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{Replay: store, Clock: clk})
	ts := unix(now.Add(time.Minute)) // future edge, accepted
	h := hdr("X-Webhook-Signature", signedAt(ts, "b"), "X-T", ts)
	if err := verify(t, v, h, "b"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if got := store.ttls[0]; got < 2*time.Minute {
		t.Fatalf("ttl = %v, want at least two tolerances", got)
	}
	// Just before the request goes stale it is still a replay, not fresh.
	clk.Advance(2*time.Minute - time.Second)
	wantErr(t, verify(t, v, h, "b"), webhookauth.ErrReplay)
	// Once stale, the timestamp refuses it whatever the store holds.
	clk.Advance(2 * time.Second)
	wantErr(t, verify(t, v, h, "b"), webhookauth.ErrStale)
}

func TestReplayTTLChoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
		min  time.Duration
		max  time.Duration
	}{
		{"no timestamp: default day", bySignature(webhookauth.Config{}), 24 * time.Hour, 24 * time.Hour},
		{"no timestamp: configured", bySignature(webhookauth.Config{Replay: webhookauth.Replay{TTL: time.Hour}}), time.Hour, time.Hour},
		{"timestamp: window", bySignature(webhookauth.Config{Timestamp: webhookauth.Timestamp{Header: "X-T", Tolerance: time.Minute}}), 2 * time.Minute, 3 * time.Minute},
		{"timestamp: short ttl is raised to the window", bySignature(webhookauth.Config{Timestamp: webhookauth.Timestamp{Header: "X-T", Tolerance: time.Minute}, Replay: webhookauth.Replay{TTL: time.Second}}), 2 * time.Minute, 3 * time.Minute},
		{"timestamp: long ttl kept", bySignature(webhookauth.Config{Timestamp: webhookauth.Timestamp{Header: "X-T", Tolerance: time.Minute}, Replay: webhookauth.Replay{TTL: time.Hour}}), time.Hour, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
			v := mustNew(t, tc.cfg, secrets(longSecret), webhookauth.Deps{Replay: store})
			h := hdr("X-Webhook-Signature", sigOf("b"))
			if tc.cfg.Timestamp.Header != "" {
				ts := unix(now)
				h = hdr("X-Webhook-Signature", signedAt(ts, "b"), "X-T", ts)
			}
			if err := verify(t, v, h, "b"); err != nil {
				t.Fatal(err)
			}
			if got := store.ttls[0]; got < tc.min || got > tc.max {
				t.Fatalf("ttl = %v, want within [%v, %v]", got, tc.min, tc.max)
			}
		})
	}
}

func TestMemoryReplayStore(t *testing.T) {
	clk := &tick{t: now}
	s := webhookauth.NewMemoryReplayStore(3, clk)
	ctx := context.Background()
	reserve := func(k string, ttl time.Duration) (bool, error) { return s.Reserve(ctx, k, ttl) }
	if fresh, err := reserve("a", time.Minute); !fresh || err != nil {
		t.Fatalf("a: %v %v", fresh, err)
	}
	if fresh, _ := reserve("a", time.Minute); fresh {
		t.Fatal("a reserved twice")
	}
	// Expiry frees the key.
	clk.Advance(time.Minute)
	if fresh, _ := reserve("a", time.Minute); !fresh {
		t.Fatal("a not fresh after its ttl")
	}
	// Full of live entries: refuse; never evict one, which would reopen a
	// replay window.
	_, _ = reserve("b", time.Hour)
	_, _ = reserve("c", time.Hour)
	_, err := reserve("d", time.Hour)
	wantErr(t, err, webhookauth.ErrReplayStoreFull)
	for _, k := range []string{"a", "b", "c"} {
		if fresh, _ := reserve(k, time.Hour); fresh {
			t.Fatalf("%s was evicted to make room", k)
		}
	}
	// Expired entries make room.
	clk.Advance(2 * time.Minute) // a expired, b and c live
	if fresh, err := reserve("d", time.Hour); !fresh || err != nil {
		t.Fatalf("d after expiry: %v %v", fresh, err)
	}
	// Release frees a slot and is idempotent.
	if err := s.Release(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "never"); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := reserve("d", time.Hour); !fresh {
		t.Fatal("d not fresh after release")
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
}

func TestMemoryReplayStoreRejectsBadTTL(t *testing.T) {
	s := webhookauth.NewMemoryReplayStore(0, clock{now})
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := s.Reserve(context.Background(), "k", ttl); err == nil {
			t.Errorf("ttl %v accepted", ttl)
		}
	}
	if _, err := s.Reserve(context.Background(), "", time.Second); err == nil {
		t.Error("empty key accepted")
	}
}

func TestMemoryReplayStoreIsBoundedUnderChurn(t *testing.T) {
	s := webhookauth.NewMemoryReplayStore(16, clock{now})
	ctx := context.Background()
	for i := 0; i < 5000; i++ {
		k := string(rune('a' + i%16))
		if _, err := s.Reserve(ctx, k, time.Hour); err != nil {
			t.Fatal(err)
		}
		_ = s.Release(ctx, k)
	}
	if n := webhookauth.QueueLen(s); n > 3*16+16 {
		t.Fatalf("expiry queue holds %d items after reserve/release churn", n)
	}
}

func TestMemoryReplayStoreConcurrentReserveHasOneWinner(t *testing.T) {
	s := webhookauth.NewMemoryReplayStore(0, clock{now})
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fresh, err := s.Reserve(context.Background(), "same", time.Hour); err == nil && fresh {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners, want 1", wins.Load())
	}
}

func TestSystemClockStoreWhenNoneInjected(t *testing.T) {
	s := webhookauth.NewMemoryReplayStore(0, nil)
	ctx := context.Background()
	if fresh, _ := s.Reserve(ctx, "k", 50*time.Millisecond); !fresh {
		t.Fatal("not fresh")
	}
	time.Sleep(80 * time.Millisecond)
	if fresh, _ := s.Reserve(ctx, "k", time.Second); !fresh {
		t.Fatal("system clock never expired the entry")
	}
}
