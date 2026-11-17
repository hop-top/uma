package storereplay_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/adapter/web/webhookauth/storereplay"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const tenant model.TenantID = "replay-test-tenant"

const secret = "0123456789abcdef0123456789abcdef"

var start = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

// backing is a memory store holding one channel, the state a restart keeps.
type backing struct {
	st    *memory.Store
	clock *portstest.FixedClock
	ch    model.ChannelRef
}

func newBacking(t *testing.T) backing {
	t.Helper()
	clock := portstest.NewFixedClock(start)
	st := memory.New(memory.Options{Clock: clock})
	ch := model.ChannelRef{Tenant: tenant, ID: "0196a000-0000-7000-8000-000000000001"}
	err := st.Transact(t.Context(), tenant, func(tx store.Tx) error {
		return tx.PutChannel(t.Context(), model.Channel{Ref: ch, Type: "test.chat", Revision: "r1"}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	return backing{st: st, clock: clock, ch: ch}
}

// open is a new process's view of the backing store.
func (b backing) open(t *testing.T, mod ...func(*storereplay.Options)) *storereplay.Store {
	t.Helper()
	o := storereplay.Options{Store: b.st, Channel: b.ch, Clock: b.clock}
	for _, m := range mod {
		m(&o)
	}
	s, err := storereplay.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func key(n int) string {
	sum := sha256.Sum256([]byte("delivery-" + strconv.Itoa(n)))
	return hex.EncodeToString(sum[:])
}

func reserve(t *testing.T, s *storereplay.Store, k string, ttl time.Duration) bool {
	t.Helper()
	fresh, err := s.Reserve(t.Context(), k, ttl)
	if err != nil {
		t.Fatalf("Reserve(%.8s): %v", k, err)
	}
	return fresh
}

func TestReserveRecordsOnce(t *testing.T) {
	s := newBacking(t).open(t)
	if !reserve(t, s, key(1), time.Hour) {
		t.Fatal("first Reserve not fresh")
	}
	if reserve(t, s, key(1), time.Hour) {
		t.Fatal("second Reserve of one key fresh")
	}
	if !reserve(t, s, key(2), time.Hour) {
		t.Fatal("a different key is not fresh")
	}
}

func TestReserveRejectsBadArguments(t *testing.T) {
	s := newBacking(t).open(t)
	for name, c := range map[string]struct {
		key string
		ttl time.Duration
	}{"empty key": {"", time.Hour}, "zero ttl": {key(1), 0}, "negative ttl": {key(1), -time.Second}} {
		if fresh, err := s.Reserve(t.Context(), c.key, c.ttl); err == nil || fresh {
			t.Errorf("%s: Reserve = %v, %v; want an error", name, fresh, err)
		}
	}
}

// TestReserveTakesAWideBucketCount: a bucket count past 32 bits picks a
// bucket like any other instead of reducing the digest by zero.
func TestReserveTakesAWideBucketCount(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int cannot hold a bucket count past 32 bits")
	}
	s := newBacking(t).open(t, func(o *storereplay.Options) { o.Buckets = 1 << 32 })
	if !reserve(t, s, key(1), time.Hour) {
		t.Fatal("first delivery refused")
	}
	if reserve(t, s, key(1), time.Hour) {
		t.Fatal("replayed delivery accepted")
	}
}

// TestRestartKeepsReservations: a store opened later over the same backing
// state, as after a process restart, still refuses a delivery recorded
// inside the window.
func TestRestartKeepsReservations(t *testing.T) {
	b := newBacking(t)
	before := b.open(t)
	if !reserve(t, before, key(1), time.Hour) {
		t.Fatal("first Reserve not fresh")
	}
	after := b.open(t) // the successor process: nothing in memory survives
	if reserve(t, after, key(1), time.Hour) {
		t.Fatal("a restarted store accepted a delivery recorded before the restart")
	}
	if !reserve(t, after, key(2), time.Hour) {
		t.Fatal("a restarted store refused a new delivery")
	}
}

// TestRestartKeepsReplayRejectionEndToEnd runs the verifier, not the store,
// across a restart: the replay of a captured request is a 409 afterwards.
func TestRestartKeepsReplayRejectionEndToEnd(t *testing.T) {
	b := newBacking(t)
	req := signed("payload")
	if _, err := verifier(t, b.open(t), "scope", b.clock).Verify(t.Context(), req); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	_, err := verifier(t, b.open(t), "scope", b.clock).Verify(t.Context(), req)
	if !errors.Is(err, webhookauth.ErrReplay) || webhookauth.Status(err) != http.StatusConflict {
		t.Fatalf("replay after restart = %v (status %d), want ErrReplay (409)", err, webhookauth.Status(err))
	}
}

func TestConcurrentReserveHasOneWinner(t *testing.T) {
	b := newBacking(t)
	stores := []*storereplay.Store{b.open(t), b.open(t), b.open(t)} // three replicas
	const keys, racers = 8, 32
	var wins [keys]atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for k := range keys {
		for r := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fresh, err := stores[r%len(stores)].Reserve(context.Background(), key(k), time.Hour)
				if err != nil {
					t.Errorf("Reserve: %v", err)
					return
				}
				if fresh {
					wins[k].Add(1)
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	for k := range keys {
		if n := wins[k].Load(); n != 1 {
			t.Errorf("key %d: %d winners, want exactly 1", k, n)
		}
	}
}

// TestConcurrentDistinctKeysShareABucket: with one bucket every reservation
// contends on one record; none may be lost or refused.
func TestConcurrentDistinctKeysShareABucket(t *testing.T) {
	b := newBacking(t)
	s := b.open(t, func(o *storereplay.Options) { o.Buckets = 1 })
	const n = 48
	var fresh atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Reserve(context.Background(), key(i), time.Hour)
			if err != nil {
				t.Errorf("Reserve: %v", err)
			}
			if ok {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != n {
		t.Fatalf("%d of %d distinct keys fresh", fresh.Load(), n)
	}
	if entries, _, err := storereplay.Dump(t.Context(), s); err != nil || entries != n {
		t.Fatalf("stored %d entries (%v), want %d", entries, err, n)
	}
}

func TestExpiryFollowsInjectedClock(t *testing.T) {
	b := newBacking(t)
	s := b.open(t)
	if !reserve(t, s, key(1), 10*time.Minute) {
		t.Fatal("first Reserve not fresh")
	}
	b.clock.Advance(10*time.Minute - time.Nanosecond)
	if reserve(t, s, key(1), 10*time.Minute) {
		t.Fatal("entry forgotten before its ttl")
	}
	b.clock.Advance(time.Nanosecond) // exactly the ttl
	if !reserve(t, b.open(t), key(1), 10*time.Minute) {
		t.Fatal("entry kept past its ttl")
	}
}

func TestReleaseFreesTheSlot(t *testing.T) {
	b := newBacking(t)
	s := b.open(t)
	reserve(t, s, key(1), time.Hour)
	reserve(t, s, key(2), time.Hour)
	if err := b.open(t).Release(t.Context(), key(1)); err != nil { // another replica
		t.Fatal(err)
	}
	if !reserve(t, s, key(1), time.Hour) {
		t.Fatal("a released key is still held")
	}
	if reserve(t, s, key(2), time.Hour) {
		t.Fatal("releasing one key freed another")
	}
	if err := s.Release(t.Context(), key(99)); err != nil {
		t.Fatalf("Release of an unknown key: %v", err)
	}
}

// TestExpiredEntriesArePurged: a write drops what expired, so storage tracks
// the live window, not the history.
func TestExpiredEntriesArePurged(t *testing.T) {
	b := newBacking(t)
	s := b.open(t, func(o *storereplay.Options) { o.Buckets = 1 })
	for i := range 50 {
		reserve(t, s, key(i), time.Minute)
	}
	if n, _, _ := storereplay.Dump(t.Context(), s); n != 50 {
		t.Fatalf("stored %d entries, want 50", n)
	}
	b.clock.Advance(time.Minute)
	reserve(t, s, key(100), time.Hour)
	if n, _, _ := storereplay.Dump(t.Context(), s); n != 1 {
		t.Fatalf("stored %d entries after expiry, want 1", n)
	}
	// Release purges too.
	reserve(t, s, key(101), time.Minute)
	b.clock.Advance(time.Minute)
	if err := s.Release(t.Context(), key(101)); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := storereplay.Dump(t.Context(), s); n != 1 {
		t.Fatalf("stored %d entries after a release, want 1", n)
	}
}

func TestStorageIsBounded(t *testing.T) {
	b := newBacking(t)
	s := b.open(t, func(o *storereplay.Options) { o.Buckets = 1; o.BucketCapacity = 3 })
	for i := range 3 {
		reserve(t, s, key(i), time.Hour)
	}
	_, err := s.Reserve(t.Context(), key(3), time.Hour)
	if !errors.Is(err, webhookauth.ErrReplayStoreFull) {
		t.Fatalf("Reserve into a full bucket = %v, want ErrReplayStoreFull", err)
	}
	if reserve(t, s, key(0), time.Hour) {
		t.Fatal("a full store forgot a live entry")
	}
	b.clock.Advance(time.Hour)
	if !reserve(t, s, key(3), time.Hour) {
		t.Fatal("expired entries did not make room")
	}
}

func TestStoresNothingSecret(t *testing.T) {
	b := newBacking(t)
	s := b.open(t)
	req := signed("a body that must not be kept")
	v := verifier(t, s, "scope", b.clock)
	if _, err := v.Verify(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	entries, raw, err := storereplay.Dump(t.Context(), s)
	if err != nil || entries != 1 {
		t.Fatalf("Dump = %d, %v", entries, err)
	}
	sig := req.Header.Get("X-Webhook-Signature")
	for name, secretText := range map[string]string{"signature": sig, "secret": secret, "body": string(req.Body), "scope": "scope"} {
		if strings.Contains(raw, secretText) {
			t.Errorf("stored state contains the %s", name)
		}
	}
}

func TestScopesDoNotShareDeliveries(t *testing.T) {
	b := newBacking(t)
	s := b.open(t)
	req := signed("same bytes")
	for _, scope := range []string{"tenant/a", "tenant/b"} {
		if _, err := verifier(t, s, scope, b.clock).Verify(t.Context(), req); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}
	if _, err := verifier(t, s, "tenant/a", b.clock).Verify(t.Context(), req); !errors.Is(err, webhookauth.ErrReplay) {
		t.Fatalf("same delivery in its own scope = %v, want ErrReplay", err)
	}
}

func TestReleaseThroughResult(t *testing.T) {
	b := newBacking(t)
	v := verifier(t, b.open(t), "scope", b.clock)
	req := signed("retry me")
	res, err := v.Verify(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier(t, b.open(t), "scope", b.clock).Verify(t.Context(), req); err != nil {
		t.Fatalf("retry of a released delivery: %v", err)
	}
}

// failingStore fails every unit.
type failingStore struct {
	store.Store
	err error
}

func (f failingStore) View(context.Context, model.TenantID, func(store.Reader) error) error {
	return f.err
}
func (f failingStore) Transact(context.Context, model.TenantID, func(store.Tx) error) error {
	return f.err
}

func TestStoreFailureRefusesTheRequest(t *testing.T) {
	b := newBacking(t)
	down := errors.New("database unreachable")
	s := b.open(t, func(o *storereplay.Options) { o.Store = failingStore{Store: b.st, err: down} })
	if fresh, err := s.Reserve(t.Context(), key(1), time.Hour); err == nil || fresh {
		t.Fatalf("Reserve on a failing store = %v, %v; want an error", fresh, err)
	}
	if err := s.Release(t.Context(), key(1)); err == nil {
		t.Fatal("Release on a failing store succeeded")
	}
	_, err := verifier(t, s, "scope", b.clock).Verify(t.Context(), signed("x"))
	if !errors.Is(err, webhookauth.ErrUnavailable) || webhookauth.Status(err) != http.StatusServiceUnavailable {
		t.Fatalf("verify on a failing store = %v (status %d), want ErrUnavailable (503)", err, webhookauth.Status(err))
	}
}

func TestMissingChannelRefusesTheRequest(t *testing.T) {
	b := newBacking(t)
	s := b.open(t, func(o *storereplay.Options) {
		o.Channel = model.ChannelRef{Tenant: tenant, ID: "0196a000-0000-7000-8000-0000000000ff"}
	})
	if fresh, err := s.Reserve(t.Context(), key(1), time.Hour); err == nil || fresh {
		t.Fatalf("Reserve without a stored channel = %v, %v; want an error", fresh, err)
	}
}

func TestCorruptStateRefusesInsteadOfResetting(t *testing.T) {
	b := newBacking(t)
	s := b.open(t, func(o *storereplay.Options) { o.Buckets = 1 })
	reserve(t, s, key(1), time.Hour)
	if err := storereplay.Corrupt(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if fresh, err := s.Reserve(t.Context(), key(1), time.Hour); err == nil || fresh {
		t.Fatalf("Reserve over corrupt state = %v, %v; want an error", fresh, err)
	}
}

// conflicting makes the first n PutProtocol calls lose a race.
type conflicting struct {
	store.Store
	left *atomic.Int32
}

func (c conflicting) Transact(ctx context.Context, tn model.TenantID, fn func(store.Tx) error) error {
	return c.Store.Transact(ctx, tn, func(tx store.Tx) error {
		return fn(racingTx{Tx: tx, left: c.left})
	})
}

type racingTx struct {
	store.Tx
	left *atomic.Int32
}

func (r racingTx) PutProtocol(ctx context.Context, s store.ProtocolState, expected model.Revision) error {
	if r.left.Add(-1) >= 0 {
		return model.NewError(model.Conflict, "stale expected revision")
	}
	return r.Tx.PutProtocol(ctx, s, expected)
}

func TestLostRaceIsRetriedThenRefused(t *testing.T) {
	b := newBacking(t)
	left := &atomic.Int32{}
	s := b.open(t, func(o *storereplay.Options) { o.Store = conflicting{Store: b.st, left: left} })

	left.Store(2)
	if !reserve(t, s, key(1), time.Hour) {
		t.Fatal("a lost race was not retried to a result")
	}
	left.Store(1 << 20) // never wins
	if fresh, err := s.Reserve(t.Context(), key(2), time.Hour); err == nil || fresh {
		t.Fatalf("endless contention = %v, %v; want an error, never fresh", fresh, err)
	}
	left.Store(0)
	if !reserve(t, s, key(2), time.Hour) {
		t.Fatal("a refused reservation left state behind")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	b := newBacking(t)
	for name, o := range map[string]storereplay.Options{
		"no store":         {Channel: b.ch},
		"no channel":       {Store: b.st},
		"negative buckets": {Store: b.st, Channel: b.ch, Buckets: -1},
		"negative cap":     {Store: b.st, Channel: b.ch, BucketCapacity: -1},
	} {
		if _, err := storereplay.New(o); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestKeysSpreadOverBuckets(t *testing.T) {
	b := newBacking(t)
	s := b.open(t, func(o *storereplay.Options) { o.Buckets = 16; o.BucketCapacity = 64 })
	for i := range 64 {
		reserve(t, s, key(i), time.Hour)
	}
	if used, err := storereplay.Used(t.Context(), s); err != nil || used < 8 {
		t.Fatalf("64 keys landed in %d of 16 buckets (%v); contention is not spread", used, err)
	}
}

// signed returns a request HMAC-signed over its timestamp and body.
func signed(body string) webhookauth.Request {
	ts := strconv.FormatInt(start.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return webhookauth.Request{
		Header: http.Header{"X-Timestamp": {ts}, "X-Webhook-Signature": {hex.EncodeToString(m.Sum(nil))}},
		Body:   []byte(body),
	}
}

func verifier(t *testing.T, s *storereplay.Store, scope string, clock *portstest.FixedClock) *webhookauth.Verifier {
	t.Helper()
	v, err := webhookauth.New(
		webhookauth.Config{
			Scheme:    webhookauth.SchemeHMAC,
			Timestamp: webhookauth.Timestamp{Header: "X-Timestamp"},
			Replay:    webhookauth.Replay{Source: webhookauth.ReplayBySignature},
		},
		webhookauth.SecretFunc(func(context.Context) ([][]byte, error) { return [][]byte{[]byte(secret)}, nil }),
		webhookauth.Deps{Clock: clock, Replay: s, Scope: scope},
	)
	if err != nil {
		t.Fatalf("webhookauth.New: %v", err)
	}
	return v
}

// overlapStore records the most units in flight at once.
type overlapStore struct {
	store.Store
	now, peak atomic.Int32
}

func (o *overlapStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	n := o.now.Add(1)
	defer o.now.Add(-1)
	for p := o.peak.Load(); n > p && !o.peak.CompareAndSwap(p, n); p = o.peak.Load() {
	}
	time.Sleep(time.Millisecond)
	return o.Store.Transact(ctx, t, fn)
}

// Writers of one bucket in one process take turns: optimistic writes to one
// record commit one per round, so racing them exhausts the retry budget of
// the unlucky when the writers outnumber the attempts.
func TestLocalWritersOfOneBucketTakeTurns(t *testing.T) {
	b := newBacking(t)
	o := &overlapStore{Store: b.st}
	s := b.open(t, func(opt *storereplay.Options) { opt.Store = o; opt.Buckets = 1 })
	const n = 64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !reserve(t, s, key(i), time.Hour) {
				t.Errorf("key %d not fresh", i)
			}
		}()
	}
	wg.Wait()
	if p := o.peak.Load(); p != 1 {
		t.Fatalf("%d units of one bucket in flight at once, want 1", p)
	}
	if entries, _, err := storereplay.Dump(t.Context(), s); err != nil || entries != n {
		t.Fatalf("entries = %d, %v; want %d", entries, err, n)
	}
}
