package main

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

// testImage is the server the smoke tests run against, pinned by
// multi-platform index digest, the same one the store's own suite uses.
const testImage = "postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"

// requireDockerEnv makes a missing or unhealthy container runtime fail the
// tests instead of skipping them. CI sets it.
const requireDockerEnv = "UMA_REQUIRE_DOCKER"

// server is the one container shared by the tests; each test gets its own
// empty database in it.
var server struct {
	once  sync.Once
	err   error
	ctr   *tcpostgres.PostgresContainer
	admin *pgxpool.Pool
	next  atomic.Int64
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
	server.admin, err = pgxpool.New(ctx, dsn)
	return err
}

// database is one empty database in the shared container, dropped when
// the test ends. Start migrates it.
type database struct {
	t    *testing.T
	name string
}

func newDatabase(t *testing.T) database {
	t.Helper()
	requireDocker(t)
	server.once.Do(func() { server.err = startServer() })
	if server.err != nil {
		t.Fatalf("postgres test server: %v", server.err)
	}
	name := fmt.Sprintf("inbox_test_%d", server.next.Add(1))
	if _, err := server.admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		drop := "DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
		if _, err := server.admin.Exec(context.Background(), drop); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	return database{t: t, name: name}
}

// pool opens a new pool on the database, as a new host process would. It
// closes when the test ends.
func (d database) pool() *pgxpool.Pool {
	d.t.Helper()
	cfg := server.admin.Config().Copy()
	cfg.ConnConfig.Database = d.name
	p, err := pgxpool.NewWithConfig(d.t.Context(), cfg)
	if err != nil {
		d.t.Fatalf("connect: %v", err)
	}
	d.t.Cleanup(p.Close)
	return p
}
