package postgres

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// testImage is the server the tests run against, pinned by multi-platform
// index digest. PostgreSQL 18 is the current major release; the schema needs
// 15 or later (UNIQUE NULLS NOT DISTINCT).
const testImage = "postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"

// requireDockerEnv makes a missing or unhealthy container runtime fail the
// tests instead of skipping them. CI sets it.
const requireDockerEnv = "UMA_REQUIRE_DOCKER"

// server is the one container shared by every test in the binary. Each test
// gets its own database in it.
var server struct {
	once     sync.Once
	err      error
	ctr      *tcpostgres.PostgresContainer
	admin    *pgxpool.Pool // connected to the maintenance database
	template string        // migrated template database name
	next     atomic.Int64
}

func TestMain(m *testing.M) {
	code := m.Run()
	if server.admin != nil {
		server.admin.Close()
	}
	if server.ctr != nil {
		if err := testcontainers.TerminateContainer(server.ctr); err != nil {
			log.Printf("terminate postgres container: %v", err)
		}
	}
	os.Exit(code)
}

// requireDocker skips t when no healthy container runtime is available,
// unless UMA_REQUIRE_DOCKER=1, which fails it.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv(requireDockerEnv) != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
		return
	}
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		t.Fatalf("%s=1 but no container provider: %v", requireDockerEnv, err)
	}
	if err := provider.Health(t.Context()); err != nil {
		t.Fatalf("%s=1 but the container provider is unhealthy: %v", requireDockerEnv, err)
	}
}

// startServer starts the shared container and migrates a template database
// that per-test databases are cloned from.
func startServer() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, testImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("uma"),
		tcpostgres.WithPassword("uma"),
		tcpostgres.BasicWaitStrategies(),
	)
	server.ctr = ctr
	if err != nil {
		return fmt.Errorf("start postgres container: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	if server.admin, err = pgxpool.New(ctx, dsn); err != nil {
		return err
	}
	server.template = "uma_template"
	if _, err := server.admin.Exec(ctx, "CREATE DATABASE "+server.template); err != nil {
		return err
	}
	tpl, err := connect(ctx, server.template)
	if err != nil {
		return err
	}
	defer tpl.Close()
	return Migrate(ctx, tpl)
}

func connect(ctx context.Context, database string) (*pgxpool.Pool, error) {
	cfg := server.admin.Config().Copy()
	cfg.ConnConfig.Database = database
	return pgxpool.NewWithConfig(ctx, cfg)
}

// newDatabase returns a pool on a new database in the shared container:
// migrated to the embedded schema when migrated is true, empty otherwise. The
// database is dropped when t ends.
func newDatabase(t *testing.T, migrated bool) *pgxpool.Pool {
	t.Helper()
	requireDocker(t)
	server.once.Do(func() { server.err = startServer() })
	if server.err != nil {
		t.Fatalf("postgres test server: %v", server.err)
	}
	ctx := t.Context()
	name := fmt.Sprintf("uma_test_%d", server.next.Add(1))
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if migrated {
		stmt += " TEMPLATE " + pgx.Identifier{server.template}.Sanitize()
	}
	if _, err := server.admin.Exec(ctx, stmt); err != nil {
		t.Fatalf("create database: %v", err)
	}
	pool, err := connect(ctx, name)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop := "DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
		if _, err := server.admin.Exec(context.Background(), drop); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	return pool
}

// NewTestDatabase is newDatabase for the external test package.
func NewTestDatabase(t *testing.T, migrated bool) *pgxpool.Pool {
	t.Helper()
	return newDatabase(t, migrated)
}

// RestartTestStore simulates a crash of the process holding s and returns a
// store opened by its successor. Every server backend on s's database is
// terminated, including one running a unit still in flight, so the database
// rolls that unit back; s's pool is closed without waiting for the units
// still holding its connections. The new store runs on a new pool against
// the same database and keeps s's clock. The new pool closes when t ends.
func RestartTestStore(t *testing.T, s *Store) *Store {
	t.Helper()
	ctx := t.Context()
	name := s.pool.Config().ConnConfig.Database
	// The two-argument form waits up to the timeout (ms) for each backend
	// to exit, so its locks are gone before the successor connects.
	if _, err := server.admin.Exec(ctx,
		`SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name); err != nil {
		t.Fatalf("terminate backends: %v", err)
	}
	// Close blocks until in-flight units return their connections; a crashed
	// process never waits for them.
	go s.pool.Close()
	pool, err := connect(ctx, name)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	t.Cleanup(pool.Close)
	next, err := Open(ctx, pool, Options{Clock: s.clock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return next
}

// NewTestPool returns a second pool on pool's database, as another process
// sharing it would hold. It closes when t ends.
func NewTestPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	p, err := connect(t.Context(), pool.Config().ConnConfig.Database)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}
