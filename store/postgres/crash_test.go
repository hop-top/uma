package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// TestServerCrashKeepsCommittedUnits kills the database server with SIGKILL,
// a unit in flight, and restarts it on the same data directory. Units
// committed before the kill survive with their claims, events, work and
// leases; the unit in flight leaves no trace, even when its process later
// tries to commit it. The test runs its own server so the kill touches no
// other test.
func TestServerCrashKeepsCommittedUnits(t *testing.T) {
	requireDocker(t)
	ctx := t.Context()
	ctr, err := tcpostgres.Run(ctx, testImage,
		tcpostgres.WithDatabase("uma"),
		tcpostgres.WithUsername("uma"),
		tcpostgres.WithPassword("uma"),
		tcpostgres.BasicWaitStrategies(),
	)
	t.Cleanup(func() {
		if ctr != nil {
			_ = ctr.Terminate(context.Background())
		}
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	pool := dial(t, ctr)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	st, err := Open(ctx, pool, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}

	ids := portstest.NewSequenceIDs(3)
	id := func() model.ID {
		v, err := ids.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const tenant model.TenantID = "crash-tenant"
	inbox := model.InboxRef{Tenant: tenant, ID: id()}
	channel := func() model.Channel {
		return model.Channel{Ref: model.ChannelRef{Tenant: tenant, ID: id()}, Type: "test.chat", Revision: "r1"}
	}
	event := func() model.Event {
		at := clock.Now()
		return model.Event{
			Version: 1, ID: id(), Kind: "postgres.test.crash", Tenant: tenant, Inbox: inbox.ID,
			Resource: model.ConversationRef{Inbox: inbox, ID: id()}.Ref(), Actor: "test-actor",
			OccurredAt: at, RecordedAt: at, Audience: model.Audience{Visibility: model.Internal},
		}
	}
	work := func() store.Work {
		return store.Work{ID: id(), Kind: "postgres.test.work", Resource: model.OperationRef{Tenant: tenant, ID: id()}.Ref()}
	}
	op := store.OperationRecord{
		Operation: model.Operation{Ref: model.OperationRef{Tenant: tenant, ID: id()}, State: model.Queued, Revision: "o1"},
		Key: store.OperationKey{
			Tenant: tenant, Caller: "test-actor", Kind: "messages.reply", Target: inbox.Ref(), ID: "op-crash",
		},
		FingerprintVersion: 1, Fingerprint: []byte{4, 2},
	}

	kept, ev, w := channel(), event(), work()
	var lease store.Lease
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, kept, ""); err != nil {
			return err
		}
		if _, _, err := tx.ClaimOperation(ctx, op); err != nil {
			return err
		}
		if err := tx.AppendEvents(ctx, []model.Event{ev}); err != nil {
			return err
		}
		if err := tx.EnqueueWork(ctx, w); err != nil {
			return err
		}
		var err error
		lease, err = tx.AcquireLease(ctx, store.LeaseRequest{Resource: op.Operation.Ref.Ref(), Owner: "worker-a", Until: clock.Now().Add(time.Minute)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	lost := channel()
	written := make(chan error, 1)
	commit := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- st.Transact(ctx, tenant, func(tx store.Tx) error {
			err := firstErr(
				tx.PutChannel(ctx, lost, ""),
				tx.AppendEvents(ctx, []model.Event{event()}),
				tx.EnqueueWork(ctx, work()),
			)
			written <- err
			<-commit
			return err
		})
	}()
	if err := <-written; err != nil {
		t.Fatalf("in-flight unit: %v", err)
	}

	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.Close() }()
	// SIGKILL: no shutdown checkpoint; the restart runs crash recovery.
	if _, err := docker.ContainerKill(ctx, ctr.GetContainerID(), client.ContainerKillOptions{Signal: "KILL"}); err != nil {
		t.Fatalf("kill server: %v", err)
	}
	// The crashed process's unit tries to commit against the dead server.
	close(commit)
	if err := <-ended; err == nil {
		t.Fatal("in-flight unit committed after the server was killed")
	}
	go pool.Close()
	if err := ctr.Start(ctx); err != nil {
		t.Fatalf("restart server: %v", err)
	}
	st, err = Open(ctx, dial(t, ctr), Options{Clock: clock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	if err := st.View(ctx, tenant, func(r store.Reader) error {
		if _, err := r.GetChannel(ctx, kept.Ref); err != nil {
			return err
		}
		if _, err := r.GetChannel(ctx, lost.Ref); model.CodeOf(err) != model.NotFound {
			t.Errorf("in-flight unit's channel = %v, want not_found", err)
		}
		p, err := r.Events(ctx, model.Filter{}, "", 10)
		if err != nil {
			return err
		}
		if len(p.Items) != 1 || p.Items[0].ID != ev.ID {
			t.Errorf("events = %d, want only the committed one", len(p.Items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		got, created, err := tx.ClaimOperation(ctx, op)
		if err != nil {
			return err
		}
		if created || got.Operation.Ref != op.Operation.Ref {
			t.Errorf("operation replay = %v created=%v, want the stored claim", got.Operation.Ref, created)
		}
		if _, err := tx.AcquireLease(ctx, store.LeaseRequest{Resource: op.Operation.Ref.Ref(), Owner: "worker-b", Until: clock.Now().Add(time.Minute)}); model.CodeOf(err) != model.Conflict {
			t.Errorf("acquire over the committed live lease = %v, want conflict", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var claimed []store.Work
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		var err error
		claimed, err = tx.ClaimWork(ctx, store.WorkClaim{Kinds: []string{w.Kind}, Limit: 10, Owner: "worker-b", Until: clock.Now().Add(time.Minute)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := workIDs(claimed); !slices.Equal(got, []model.ID{w.ID}) {
		t.Fatalf("claimable work = %v, want only the committed item %s", got, w.ID)
	}
	clock.Advance(2 * time.Minute)
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		next, err := tx.AcquireLease(ctx, store.LeaseRequest{Resource: op.Operation.Ref.Ref(), Owner: "worker-b", Until: clock.Now().Add(time.Minute)})
		if err == nil && next.Fence.Generation <= lease.Fence.Generation {
			t.Errorf("generation after the crash = %d, want above %d", next.Fence.Generation, lease.Fence.Generation)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// dial connects to ctr, waiting for a server that is still recovering.
func dial(t *testing.T, ctr *tcpostgres.PostgresContainer) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for {
		pool, err := connectContainer(ctx, ctr)
		if err == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		select {
		case <-ctx.Done():
			t.Fatalf("connect: %v", errors.Join(err, ctx.Err()))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func connectContainer(ctx context.Context, ctr *tcpostgres.PostgresContainer) (*pgxpool.Pool, error) {
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func workIDs(ws []store.Work) []model.ID {
	out := make([]model.ID, len(ws))
	for i, w := range ws {
		out[i] = w.ID
	}
	return out
}
