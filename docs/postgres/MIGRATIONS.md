# PostgreSQL schema migrations

`hop.top/uma/store/postgres` embeds its schema as numbered SQL migrations.
The host runs them; the store never migrates on its own.

## What you need

- PostgreSQL 15 or later (the schema uses `UNIQUE NULLS NOT DISTINCT`). The
  tests run against PostgreSQL 18.
- A role that can create tables in the connection's `search_path` schema while
  migrating. Opening and using the store needs only read and write access to
  the `uma_` tables.

## Run migrations, then open

```go
if err := postgres.Migrate(ctx, pool); err != nil {
	return err
}
st, err := postgres.Open(ctx, pool, postgres.Options{})
```

- `Migrate` applies every embedded migration the database lacks. It holds a
  PostgreSQL session advisory lock while it runs, so several processes
  starting together migrate once. Running it again is a no-op.
- `Open` checks the schema version and fails with a `schema_mismatch` error
  when the database is older or newer than the package. It never migrates.
- Neither closes the pool you pass.

The applied versions live in `uma_schema_migrations`, apart from any
migration history your application keeps in the same database. Every uma
table is prefixed `uma_`. To keep uma in its own schema, set `search_path` on
the pool's connections.

## Upgrade order

A newer package may add migrations. Upgrade in this order:

1. Back up the database.
2. Stop every process using the old package version, or make sure none is
   running when step 3 commits: an old process does not re-check the schema
   once it has opened the store.
3. Run `Migrate` once with the new package version, for example from a
   deploy step or the first new process to start.
4. Start the new version everywhere. Each process calls `Open`, which accepts
   only the schema version its package embeds.

Never point an older package at a migrated database: its `Open` fails with
`schema_mismatch` and the message says the schema is newer. `Migrate` from an
older package fails the same way and changes nothing. Downgrading means
restoring the backup taken in step 1.

## Store version and package version

`postgres.SchemaVersion` and `Store.Properties().SchemaVersion` report the
newest embedded migration. It changes only when a release adds a migration.
