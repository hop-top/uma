// Package postgres is the PostgreSQL store.Store for uma: a versioned
// schema with explicit migrations, generated queries and transaction-scoped
// units of work on a host-owned pgx pool.
//
// # Setup
//
// The host owns the *pgxpool.Pool and runs migrations explicitly, before
// opening the store:
//
//	if err := postgres.Migrate(ctx, pool); err != nil { ... }
//	st, err := postgres.Open(ctx, pool, postgres.Options{})
//
// Open never migrates. It fails with a model.SchemaMismatch error when the
// database schema is older or newer than the one this package embeds; see
// docs/postgres/MIGRATIONS.md in the uma repository for the upgrade order.
//
// # Units of work
//
// Transact runs its callback in a SERIALIZABLE transaction; View in a
// REPEATABLE READ, READ ONLY one. The one exception is a claim unit, a
// Transact unit whose first call is ClaimWork: it runs at READ COMMITTED,
// so concurrent claimers skip each other's items instead of aborting, and
// it may make ClaimWork calls only; any other call fails with
// model.Validation and the unit rolls back. A unit that loses a
// serialization race fails with model.Transient and the callback is not
// rerun; the engine may retry the complete unit. A database error inside a unit aborts the
// PostgreSQL transaction: every later call on that unit returns the same
// error and the unit never commits, even when the callback returns nil.
//
// # Durability and concurrency
//
// Properties reports Durable=true. A committed unit survives a lost
// connection, a closed pool and a killed server; a unit still open when its
// process or the server stops leaves no trace, and readers never see a unit's
// writes before it commits. Processes may share one database through their
// own pools:
//
//   - A unit appends events at placeholders and writes no row other
//     appenders write, so appenders to one tenant do not fail one another.
//     Positions are assigned once the unit commits, one placement at a time
//     per tenant (a transaction-scoped advisory lock), together with the
//     tenant's head row: positions are dense and in the order events
//     become readable, a unit's events adjacent, and a tail reading from
//     its last cursor never skips a unit that commits late. A unit that
//     reads the log after appending places its own events inside the unit.
//   - New operation, identity, conversation, message and ingress claims,
//     new messages, contacts and work are inserted with no prior lookup of
//     their keys, as events are: a read would leave a predicate lock on the
//     index page that new, time-ordered keys share, and units writing new
//     rows of one tenant would fail one another on it. Only a write that
//     finds its key held reads it; a key a concurrent unit committed fails
//     the unit with model.Transient. The foreign keys of the rows written
//     with a new message or conversation are checked at commit, after the
//     unit's writes.
//   - The reads of a unit that precede its writes, the references of a
//     provider message ID, the claims of a conversation group, a
//     conversation by key, probe by key and fetch what they name by key:
//     none scans a range of the tenant's rows, which every new row of the
//     tenant is written into. A probe of a key no row holds still locks the
//     index page the key belongs to, so units whose keys share a page, as
//     time-ordered keys and the keys of a small table do, can fail one
//     another though they write different rows.
//   - ClaimWork skips work rows another unit is claiming instead of waiting
//     for them. It takes a lease only from an expired holder, rechecked
//     under the lease row's lock, and counts an in-flight cap under a
//     transaction-scoped advisory lock per tenant and work kind (a
//     hashtextextended key). Leases carry a generation that only grows; a
//     stale fence never completes work, renews or releases a lease, or
//     updates an operation.
//   - An interaction's ingress looks up the message holding its control's
//     provider message ID, and the unit recording a sent message's provider
//     IDs looks up the interactions still unlinked on them (a partial index
//     of unlinked rows). Each reads what the other writes, so of two such
//     units that overlap one fails with model.Transient and, rerun, links
//     the interaction the other recorded.
//   - Concurrent claims of one identity, conversation, ingress key or
//     operation converge on one stored claim: a unit that loses the race
//     fails with model.Transient and replays the stored claim when retried.
//     Of concurrent units leasing one resource, at most one commits.
//
// # Representation
//
// Tables are prefixed uma_ and live in the connection's search_path schema.
// Internal IDs are uuid columns; tenants are text keys. Each table holds
// explicit identity, constraint, filter and ordering columns and the
// complete record as JSON in a body column. Lists page in insertion order;
// the event log pages in commit order per tenant.
//
// An interaction's response token is the exception: it lives in its own
// column of uma_interactions, never in the body. Only the query behind
// Reader.InteractionToken selects it; redaction and the expired-token
// sweep clear it in place.
package postgres
