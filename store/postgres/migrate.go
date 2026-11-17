package postgres

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"hop.top/uma/model"
)

// SchemaVersion is the storage schema version this package embeds: the
// version of its newest migration.
const SchemaVersion = 26

// VersionTable is the table recording applied schema migrations. It is
// separate from a host's own migration history in the same database.
const VersionTable = "uma_schema_migrations"

//go:embed migrations/*.sql
var embedded embed.FS

func migrations() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}

// Migrate applies every embedded migration the database lacks, under a
// session advisory lock, so concurrent hosts migrate once. It then fills
// the message search tokens of rows written before the search migration,
// which SQL cannot compute; rerunning Migrate finishes an interrupted
// fill. It fails with model.SchemaMismatch when the database is already at
// a newer schema than this package embeds. It never closes pool.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, migrations())
}

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	if pool == nil {
		return model.NewError(model.Validation, "pool: required")
	}
	var target int64
	if err := withProvider(ctx, pool, fsys, func(p *goose.Provider) error {
		current, last, err := p.GetVersions(ctx)
		if err != nil {
			return migrationError(ctx, err)
		}
		if current > last {
			return newerSchema(current, last)
		}
		if _, err := p.Up(ctx); err != nil {
			return migrationError(ctx, err)
		}
		target = last
		return nil
	}); err != nil {
		return err
	}
	if target < searchVersion {
		return nil
	}
	return fillSearchTokens(ctx, pool)
}

// checkSchema requires the database schema to be exactly the embedded one,
// its message search tokens filled.
func checkSchema(ctx context.Context, pool *pgxpool.Pool) error {
	return checkSchemaFS(ctx, pool, migrations())
}

func checkSchemaFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	return withProvider(ctx, pool, fsys, func(p *goose.Provider) error {
		current, target, err := p.GetVersions(ctx)
		if err != nil {
			return migrationError(ctx, err)
		}
		switch {
		case current > target:
			return newerSchema(current, target)
		case current < target:
			return model.NewError(model.SchemaMismatch,
				"database schema version "+strconv.FormatInt(current, 10)+" is older than "+
					strconv.FormatInt(target, 10)+"; run Migrate before Open")
		case target >= searchVersion:
			return checkSearchFilled(ctx, pool)
		}
		return nil
	})
}

func newerSchema(current, target int64) error {
	return model.NewError(model.SchemaMismatch,
		"database schema version "+strconv.FormatInt(current, 10)+" is newer than "+
			strconv.FormatInt(target, 10)+"; upgrade this package before opening the database")
}

// withProvider runs fn with a goose provider on a database/sql wrapper of
// pool. Closing the wrapper releases its connections to pool and leaves pool
// open.
func withProvider(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, fn func(*goose.Provider) error) (err error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return model.NewError(model.Permanent, "migration lock unavailable")
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() {
		if cerr := sqlDB.Close(); cerr != nil && err == nil {
			err = migrationError(ctx, cerr)
		}
	}()
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys,
		goose.WithDisableGlobalRegistry(true),
		goose.WithSessionLocker(locker),
		goose.WithTableName(VersionTable),
	)
	if err != nil {
		return model.NewError(model.Permanent, "migrations cannot be loaded")
	}
	return fn(p)
}

// migrationError classifies a goose failure. goose wraps driver errors, so
// classify sees the underlying database error when there is one.
func migrationError(ctx context.Context, err error) error {
	var me *model.Error
	if errors.As(err, &me) {
		return me
	}
	return classify(ctx, err)
}
