package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/adapter/web/webhookauth/storereplay"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// replayImage is the server the replay tests run against: the image the
// rest of the suite pins, unless UMA_REPLAY_TEST_IMAGE names another one
// (a local one avoids a pull). PostgreSQL 15 or later is required.
const replayImage = "postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"

// replayServer starts a PostgreSQL container that only these tests use: a
// name starting uma-replay-, the label uma.replay-test=1, a port bound to
// the loopback interface, and removal when t ends. It returns the DSN.
func replayServer(t *testing.T) string {
	t.Helper()
	// No reaper container: the one container is removed by this test.
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	if os.Getenv("UMA_REQUIRE_DOCKER") == "1" {
		provider, err := testcontainers.ProviderDocker.GetProvider()
		if err != nil {
			t.Fatalf("UMA_REQUIRE_DOCKER=1 but no container provider: %v", err)
		}
		if err := provider.Health(t.Context()); err != nil {
			t.Fatalf("UMA_REQUIRE_DOCKER=1 but the container provider is unhealthy: %v", err)
		}
	} else {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	image := replayImage
	if v := os.Getenv("UMA_REPLAY_TEST_IMAGE"); v != "" {
		image = v
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("uma_replay"),
		tcpostgres.WithUsername("uma"),
		tcpostgres.WithPassword("uma"),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithName(fmt.Sprintf("uma-replay-%d-%d", os.Getpid(), time.Now().UnixNano())),
		testcontainers.WithLabels(map[string]string{"uma.replay-test": "1"}),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("5432/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "0"}},
			}
		}),
	)
	if ctr != nil {
		t.Cleanup(func() {
			if err := testcontainers.TerminateContainer(ctr); err != nil {
				t.Errorf("remove replay container: %v", err)
			}
		})
	}
	if err != nil {
		t.Fatalf("start replay container: %v", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// replayDB is one database of the replay container: a migrated schema, one
// stored channel, and a way to open stores on fresh pools, as separate
// processes sharing it would.
type replayDB struct {
	dsn   string
	ch    model.ChannelRef
	clock *portstest.FixedClock
}

func (d replayDB) pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(t.Context(), d.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// replica is a new process: its own pool, store and replay store.
func (d replayDB) replica(t *testing.T, pool *pgxpool.Pool, buckets, capacity int) *storereplay.Store {
	t.Helper()
	st, err := postgres.Open(t.Context(), pool, postgres.Options{Clock: d.clock})
	if err != nil {
		t.Fatal(err)
	}
	s, err := storereplay.New(storereplay.Options{Store: st, Channel: d.ch, Clock: d.clock, Buckets: buckets, BucketCapacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newReplayDB(t *testing.T, dsn string, n int) replayDB {
	t.Helper()
	ctx := t.Context()
	admin := func() *pgxpool.Pool {
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}()
	defer admin.Close()
	name := "replay_" + strconv.Itoa(n)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn = u.String()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := postgres.Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	d := replayDB{
		dsn:   dsn,
		clock: portstest.NewFixedClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)),
		ch:    model.ChannelRef{Tenant: "replay-pg-tenant", ID: "0196a000-0000-7000-8000-000000000001"},
	}
	st, err := postgres.Open(ctx, p, postgres.Options{Clock: d.clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Transact(ctx, d.ch.Tenant, func(tx store.Tx) error {
		return tx.PutChannel(ctx, model.Channel{Ref: d.ch, Type: "test.chat", Revision: "r1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	return d
}

func replayKey(n int) string {
	sum := sha256.Sum256([]byte("delivery-" + strconv.Itoa(n)))
	return hex.EncodeToString(sum[:])
}

// storedBuckets returns the number of replay records and of entries in them.
func storedBuckets(t *testing.T, pool *pgxpool.Pool) (records, entries int) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT value_data::text FROM uma_protocol_states WHERE key LIKE 'webhookauth.replay/%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var st struct {
			Entries map[string]int64 `json:"entries"`
		}
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		records++
		entries += len(st.Entries)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return records, entries
}

// TestReplayStore runs the durable replay store against a real PostgreSQL
// server: the behaviours that need real transactions, a real restart and
// several connection pools.
func TestReplayStore(t *testing.T) {
	dsn := replayServer(t)
	var dbs int
	fresh := func(t *testing.T) replayDB {
		dbs++
		return newReplayDB(t, dsn, dbs)
	}

	t.Run("restart keeps a reservation", func(t *testing.T) {
		d := fresh(t)
		ctx := t.Context()
		first := d.pool(t)
		if ok, err := d.replica(t, first, 0, 0).Reserve(ctx, replayKey(1), time.Hour); err != nil || !ok {
			t.Fatalf("first Reserve = %v, %v", ok, err)
		}
		first.Close() // the process stops; nothing of it remains
		successor := d.replica(t, d.pool(t), 0, 0)
		if ok, err := successor.Reserve(ctx, replayKey(1), time.Hour); err != nil || ok {
			t.Fatalf("Reserve after a restart = %v, %v; want a duplicate", ok, err)
		}
		if ok, err := successor.Reserve(ctx, replayKey(2), time.Hour); err != nil || !ok {
			t.Fatalf("a new delivery after a restart = %v, %v", ok, err)
		}
	})

	t.Run("one winner among replicas", func(t *testing.T) {
		d := fresh(t)
		replicas := []*storereplay.Store{
			d.replica(t, d.pool(t), 0, 0), d.replica(t, d.pool(t), 0, 0), d.replica(t, d.pool(t), 0, 0),
		}
		const keys, racers = 6, 24
		var wins [keys]atomic.Int32
		var failed atomic.Int32
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for k := range keys {
			for r := range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate
					ok, err := replicas[r%len(replicas)].Reserve(context.Background(), replayKey(k), time.Hour)
					switch {
					case err != nil:
						failed.Add(1)
						t.Errorf("Reserve: %v", err)
					case ok:
						wins[k].Add(1)
					}
				}()
			}
		}
		close(gate)
		wg.Wait()
		for k := range keys {
			if n := wins[k].Load(); n != 1 {
				t.Errorf("key %d: %d winners, want exactly 1", k, n)
			}
		}
	})

	t.Run("distinct keys sharing a record are all kept", func(t *testing.T) {
		d := fresh(t)
		pool := d.pool(t)
		replicas := []*storereplay.Store{d.replica(t, pool, 1, 0), d.replica(t, d.pool(t), 1, 0)}
		const n = 40
		var ok atomic.Int32
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fresh, err := replicas[i%2].Reserve(context.Background(), replayKey(i), time.Hour)
				if err != nil {
					t.Errorf("Reserve: %v", err)
				}
				if fresh {
					ok.Add(1)
				}
			}()
		}
		wg.Wait()
		records, entries := storedBuckets(t, pool)
		if ok.Load() != n || records != 1 || entries != n {
			t.Fatalf("%d of %d fresh, %d records holding %d entries; want all fresh in 1 record of %d", ok.Load(), n, records, entries, n)
		}
	})

	t.Run("expiry release and purge", func(t *testing.T) {
		d := fresh(t)
		pool := d.pool(t)
		s := d.replica(t, pool, 1, 0)
		ctx := t.Context()
		for i := range 30 {
			if ok, err := s.Reserve(ctx, replayKey(i), 10*time.Minute); err != nil || !ok {
				t.Fatalf("Reserve %d = %v, %v", i, ok, err)
			}
		}
		d.clock.Advance(10*time.Minute - time.Nanosecond)
		if ok, _ := s.Reserve(ctx, replayKey(0), time.Hour); ok {
			t.Fatal("entry forgotten before its ttl")
		}
		d.clock.Advance(time.Nanosecond)
		if ok, err := d.replica(t, d.pool(t), 1, 0).Reserve(ctx, replayKey(0), time.Hour); err != nil || !ok {
			t.Fatalf("entry kept past its ttl: %v, %v", ok, err)
		}
		if records, entries := storedBuckets(t, pool); records != 1 || entries != 1 {
			t.Fatalf("%d records holding %d entries after expiry, want 1 holding 1", records, entries)
		}
		if err := s.Release(ctx, replayKey(0)); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Reserve(ctx, replayKey(0), time.Hour); err != nil || !ok {
			t.Fatalf("a released key is still held: %v, %v", ok, err)
		}
	})

	t.Run("storage is bounded by the bucket count", func(t *testing.T) {
		d := fresh(t)
		pool := d.pool(t)
		s := d.replica(t, pool, 4, 1000)
		for round := range 5 {
			for i := range 200 {
				if _, err := s.Reserve(t.Context(), replayKey(round*1000+i), time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			d.clock.Advance(time.Minute)
		}
		if records, entries := storedBuckets(t, pool); records > 4 || entries > 200 {
			t.Fatalf("%d records and %d entries after 1000 deliveries, want at most 4 and 200", records, entries)
		}
	})

	t.Run("a database that cannot be reached refuses the request", func(t *testing.T) {
		d := fresh(t)
		pool := d.pool(t)
		s := d.replica(t, pool, 0, 0)
		v, err := webhookauth.New(
			webhookauth.Config{
				Scheme: webhookauth.SchemeBearer, Replay: webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Id", AllowUnsigned: true},
			},
			webhookauth.SecretFunc(func(context.Context) ([][]byte, error) {
				return [][]byte{[]byte("0123456789abcdef0123456789abcdef")}, nil
			}),
			webhookauth.Deps{Clock: d.clock, Replay: s, Scope: "s"},
		)
		if err != nil {
			t.Fatal(err)
		}
		req := webhookauth.Request{Header: http.Header{"Authorization": {"Bearer 0123456789abcdef0123456789abcdef"}, "X-Id": {"1"}}}
		pool.Close()
		_, err = v.Verify(t.Context(), req)
		if !errors.Is(err, webhookauth.ErrUnavailable) || webhookauth.Status(err) != http.StatusServiceUnavailable {
			t.Fatalf("verify without a database = %v (status %d), want ErrUnavailable (503)", err, webhookauth.Status(err))
		}
	})
}
