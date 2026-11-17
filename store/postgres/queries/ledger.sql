-- Operation ledger, attempt evidence, the event log, leases and work.

-- name: GetOperation :one
SELECT body, revision, written_at FROM uma_operations WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetOperationByKey :one
SELECT body, written_at
FROM uma_operations
WHERE tenant_id = @tenant_id AND caller = @caller AND kind = @kind AND target_kind = @target_kind
  AND target_inbox IS NOT DISTINCT FROM sqlc.narg(target_inbox)::uuid AND target_id = @target_id
  AND operation_key = @operation_key;

-- name: InsertOperation :execrows
-- Inserts the claim unless its operation key or reference is held in the
-- unit's snapshot: no row is inserted then. No lookup precedes it, so it
-- leaves no predicate lock for concurrent claims' inserts to conflict
-- with; a key or reference a concurrent unit committed fails this unit as
-- a serialization failure.
INSERT INTO uma_operations (
    tenant_id, id, caller, kind, target_kind, target_inbox, target_id, operation_key,
    fingerprint_version, fingerprint, state, expires_at, body, revision, written_at, inbox_id, conversation_id
) VALUES (
    @tenant_id, @id, @caller, @kind, @target_kind, sqlc.narg(target_inbox)::uuid, @target_id, @operation_key,
    @fingerprint_version, @fingerprint, @state, sqlc.narg(expires_at)::timestamptz, @body, @revision, @written_at,
    sqlc.narg(inbox_id)::uuid, sqlc.narg(conversation_id)::uuid
)
ON CONFLICT DO NOTHING;

-- name: UpdateOperation :execrows
UPDATE uma_operations
SET state = @state, body = @body, revision = @revision, written_at = @written_at,
    inbox_id = sqlc.narg(inbox_id)::uuid, conversation_id = sqlc.narg(conversation_id)::uuid
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: ListOperations :many
-- Pages most recently written first. The caller passes a position past every
-- row for the first page, so the keyset comparison is always present and a
-- plan can range over the (written_at, id) indexes. One inbox or one state
-- arrives as an equality (inbox, state), which those indexes serve in order;
-- several arrive as lists (inboxes, states), which match through the same
-- indexes but need a sort.
SELECT written_at, id, body
FROM uma_operations
WHERE tenant_id = @tenant_id
  AND (written_at, id) < (@after_at::timestamptz, @after_id::uuid)
  AND (sqlc.narg(inbox)::uuid IS NULL OR inbox_id = sqlc.narg(inbox)::uuid)
  AND (cardinality(@inboxes::text[]) = 0 OR inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (sqlc.narg(conversation_inbox)::uuid IS NULL
       OR (inbox_id = sqlc.narg(conversation_inbox)::uuid AND conversation_id = sqlc.narg(conversation_id)::uuid))
  AND (sqlc.narg(state)::text IS NULL OR state = sqlc.narg(state)::text)
  AND (cardinality(@states::text[]) = 0 OR state = ANY (@states::text[]))
  AND (sqlc.narg(since)::timestamptz IS NULL OR written_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(before)::timestamptz IS NULL OR written_at < sqlc.narg(before)::timestamptz)
ORDER BY written_at DESC, id DESC
LIMIT @lim;

-- name: InsertAttempt :exec
INSERT INTO uma_attempt_evidence (tenant_id, operation_id, attempt_id, body, written_at)
VALUES (@tenant_id, @operation_id, @attempt_id, @body, @written_at);

-- name: ListAttempts :many
SELECT seq, body
FROM uma_attempt_evidence
WHERE tenant_id = @tenant_id AND operation_id = @operation_id AND seq > @after
ORDER BY seq
LIMIT @lim;

-- name: EventHead :one
SELECT COALESCE((SELECT position FROM uma_event_heads WHERE tenant_id = @tenant_id), 0)::bigint;

-- name: AdvanceEventHead :one
-- Reserves n positions and returns the new head: a unit placing its own
-- events inside the unit, under LockEventPlacement. Writing the head row
-- fails the unit as a serialization failure when a placement committed
-- after its snapshot.
INSERT INTO uma_event_heads (tenant_id, position)
VALUES (@tenant_id, @n::bigint)
ON CONFLICT (tenant_id) DO UPDATE SET position = uma_event_heads.position + excluded.position
RETURNING position;

-- name: LockEventPlacement :exec
-- Serializes the placement of one tenant's events until commit. The key
-- hashes the tenant under a seed of its own (0x756d6165, "umae"), apart
-- from LockWorkKind's.
SELECT pg_advisory_xact_lock(hashtextextended(@tenant_id::text, 1970102629));

-- name: PlaceCommittedEvents :exec
-- Places every committed event of the tenant still at a placeholder: the
-- positions after the head, unit by unit (the inserting transaction, oldest
-- first) and in placeholder order, which is append order, within a unit,
-- with the running RecordedAt maximum continued from the head row's; then
-- advances the head row to the last of them. The caller holds
-- LockEventPlacement at READ COMMITTED, so this statement's snapshot,
-- taken after the lock, holds the latest head and every unit committed
-- before it.
WITH prior AS (
    SELECT COALESCE(max(h.position), 0)::bigint AS head,
        COALESCE(max(h.recorded_max_unix::numeric * 1000000000 + h.recorded_max_nanos), -62135596800000000000) AS recorded_max
    FROM uma_event_heads h
    WHERE h.tenant_id = @tenant_id
), pending AS (
    SELECT e.position AS placeholder,
        p.head + row_number() OVER w AS position,
        greatest(p.recorded_max, max(e.recorded_unix::numeric * 1000000000 + e.recorded_nanos) OVER w) AS recorded_max
    FROM uma_events e
    CROSS JOIN prior p
    WHERE e.tenant_id = @tenant_id AND e.position < 0
    WINDOW w AS (ORDER BY age(e.xmin) DESC, e.position DESC ROWS UNBOUNDED PRECEDING)
), placed AS (
    UPDATE uma_events e
    SET position = p.position,
        recorded_max_unix = floor(p.recorded_max / 1000000000),
        recorded_max_nanos = p.recorded_max - floor(p.recorded_max / 1000000000) * 1000000000
    FROM pending p
    WHERE e.tenant_id = @tenant_id AND e.position = p.placeholder
    RETURNING p.position, p.recorded_max
)
INSERT INTO uma_event_heads (tenant_id, position, recorded_max_unix, recorded_max_nanos)
SELECT @tenant_id, max(position),
    floor(max(recorded_max) / 1000000000),
    max(recorded_max) - floor(max(recorded_max) / 1000000000) * 1000000000
FROM placed
HAVING count(*) > 0
ON CONFLICT (tenant_id) DO UPDATE
SET position = excluded.position, recorded_max_unix = excluded.recorded_max_unix,
    recorded_max_nanos = excluded.recorded_max_nanos;

-- name: UnplacedTenants :many
-- The tenants holding events at placeholders, through
-- uma_events_placeholders.
SELECT DISTINCT tenant_id FROM uma_events WHERE position < 0;

-- name: RaiseEventRecordedMax :one
-- Raises the tenant's running RecordedAt maximum to (@max_unix, @max_nanos)
-- when that is later, and returns the maximum before the call. The caller
-- places its own events after AdvanceEventHead, holding the head row, so
-- placements raise it in position order.
UPDATE uma_event_heads h
SET recorded_max_unix = CASE WHEN (@max_unix::bigint, @max_nanos::integer) > (prior.recorded_max_unix, prior.recorded_max_nanos)
        THEN @max_unix::bigint ELSE prior.recorded_max_unix END,
    recorded_max_nanos = CASE WHEN (@max_unix::bigint, @max_nanos::integer) > (prior.recorded_max_unix, prior.recorded_max_nanos)
        THEN @max_nanos::integer ELSE prior.recorded_max_nanos END
FROM uma_event_heads prior
WHERE h.tenant_id = @tenant_id AND prior.tenant_id = h.tenant_id
RETURNING prior.recorded_max_unix, prior.recorded_max_nanos;

-- name: InsertEvent :one
-- Inserts the event at its placeholder, the next from
-- uma_event_placeholders negated, and returns the placeholder. The running
-- maximum holds the event's own RecordedAt until the event is placed. No
-- row returns when the tenant's log already holds the ID in the unit's
-- snapshot. Being one statement with no prior lookup, it leaves no
-- predicate lock for other appenders' inserts to conflict with; an ID a
-- concurrent unit committed fails this unit as a serialization failure.
INSERT INTO uma_events (tenant_id, position, id, kind, inbox_id, body, written_at, conversation_id,
    recorded_unix, recorded_nanos, recorded_max_unix, recorded_max_nanos)
VALUES (@tenant_id, -nextval('uma_event_placeholders'), @id, @kind, sqlc.narg(inbox_id)::uuid, @body, @written_at,
    sqlc.narg(conversation_id)::uuid, @recorded_unix, @recorded_nanos, @recorded_unix, @recorded_nanos)
ON CONFLICT (tenant_id, id) DO NOTHING
RETURNING position;

-- name: DeleteUnplacedEvents :exec
-- Removes the unit's own events at the given placeholders.
DELETE FROM uma_events WHERE tenant_id = @tenant_id AND position = ANY (@placeholders::bigint[]);

-- name: PlaceEvents :exec
-- Moves the unit's own events from their placeholders to positions with
-- their running RecordedAt maximum, element by element.
UPDATE uma_events e
SET position = u.position, recorded_max_unix = u.max_unix, recorded_max_nanos = u.max_nanos
FROM (
    SELECT unnest(@placeholders::bigint[]) AS placeholder, unnest(@positions::bigint[]) AS position,
        unnest(@max_unix::bigint[]) AS max_unix, unnest(@max_nanos::integer[]) AS max_nanos
) u
WHERE e.tenant_id = @tenant_id AND e.position = u.placeholder;

-- name: ListEvents :many
SELECT position, body
FROM uma_events
WHERE tenant_id = @tenant_id AND position > @after AND position <= @head
  AND (cardinality(@inboxes::text[]) = 0 OR inbox_id = ANY ((@inboxes::text[])::uuid[])
       OR (body->'resource'->>'kind' = 'inbox' AND body->'resource'->>'id' = ANY (@inboxes::text[])))
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY (@kinds::text[]))
  AND (sqlc.narg(since_unix)::bigint IS NULL
       OR (recorded_unix, recorded_nanos) >= (sqlc.narg(since_unix)::bigint, sqlc.narg(since_nanos)::integer))
ORDER BY position
LIMIT @lim;

-- name: ListConversationEvents :many
-- ListEvents for one conversation: a range scan of uma_events_conversation
-- in position order. An inbox filter is the caller's to apply: every event
-- of the conversation lies in its inbox.
SELECT position, body
FROM uma_events
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id
  AND position > @after AND position <= @head
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY (@kinds::text[]))
  AND (sqlc.narg(since_unix)::bigint IS NULL
       OR (recorded_unix, recorded_nanos) >= (sqlc.narg(since_unix)::bigint, sqlc.narg(since_nanos)::integer))
ORDER BY position
LIMIT @lim;

-- name: EventSinceStart :many
-- The first position whose running RecordedAt maximum reaches since; no
-- row when none does. No event before it was recorded at or after since.
-- The running maximum only grows along the log, so the first placed row of
-- uma_events_recorded_max at or after since holds the least such position.
SELECT position FROM uma_events
WHERE tenant_id = @tenant_id
  AND (recorded_max_unix, recorded_max_nanos) >= (@since_unix::bigint, @since_nanos::integer)
  AND position > 0
ORDER BY recorded_max_unix, recorded_max_nanos, position
LIMIT 1;

-- name: GetLease :one
SELECT generation, owner, until
FROM uma_leases
WHERE tenant_id = @tenant_id AND resource_kind = @resource_kind
  AND resource_inbox IS NOT DISTINCT FROM sqlc.narg(resource_inbox)::uuid AND resource_id = @resource_id;

-- name: ClaimLease :one
-- Leases the resource under its next generation, creating its row on first
-- use, only while it holds no unexpired lease at @now: ClaimWork and
-- AcquireLease. The conflicting row is locked and rechecked at its latest
-- committed version (READ COMMITTED), or a version newer than the unit's
-- snapshot fails it as a serialization failure (SERIALIZABLE), so a lease
-- another unit committed concurrently is never taken over. No row means the
-- resource is leased. Being one statement, it leaves no predicate lock from
-- a prior lookup for takes of other resources to conflict with.
INSERT INTO uma_leases (tenant_id, resource_kind, resource_inbox, resource_id, generation, owner, until)
VALUES (@tenant_id, @resource_kind, sqlc.narg(resource_inbox)::uuid, @resource_id, 1, @owner::text, @until::timestamptz)
ON CONFLICT (tenant_id, resource_kind, resource_inbox, resource_id)
DO UPDATE SET generation = uma_leases.generation + 1, owner = excluded.owner, until = excluded.until
WHERE uma_leases.until IS NULL OR uma_leases.until <= @now::timestamptz
RETURNING generation;

-- name: LockWorkKind :exec
-- Serializes in-flight counts of one tenant's work kind until commit. The
-- key hashes tenant and kind under a fixed seed (0x756d61, "uma").
SELECT pg_advisory_xact_lock(hashtextextended(@tenant_id::text || chr(31) || @kind::text, 7695713));

-- name: RenewLease :execrows
UPDATE uma_leases
SET until = @until::timestamptz
WHERE tenant_id = @tenant_id AND resource_kind = @resource_kind
  AND resource_inbox IS NOT DISTINCT FROM sqlc.narg(resource_inbox)::uuid AND resource_id = @resource_id
  AND owner = @owner::text AND generation = @generation;

-- name: ReleaseLease :execrows
UPDATE uma_leases
SET owner = NULL, until = NULL
WHERE tenant_id = @tenant_id AND resource_kind = @resource_kind
  AND resource_inbox IS NOT DISTINCT FROM sqlc.narg(resource_inbox)::uuid AND resource_id = @resource_id
  AND owner = @owner::text AND generation = @generation;

-- name: WorkExists :one
SELECT EXISTS (SELECT 1 FROM uma_work WHERE tenant_id = @tenant_id AND id = @id);

-- name: InsertWork :execrows
-- Inserts the work unless the tenant already holds its ID in the unit's
-- snapshot: no row is inserted then. No lookup precedes it, so it leaves
-- no predicate lock for concurrent enqueuers' inserts to conflict with; an
-- ID a concurrent unit committed fails this unit as a serialization
-- failure.
INSERT INTO uma_work (tenant_id, id, kind, resource_kind, resource_inbox, resource_id, not_before)
VALUES (@tenant_id, @id, @kind, @resource_kind, sqlc.narg(resource_inbox)::uuid, @resource_id,
        sqlc.narg(not_before)::timestamptz)
ON CONFLICT (tenant_id, id) DO NOTHING;

-- name: ListPendingWork :many
-- Unquarantined work of the given kinds in enqueue order, not deferred past
-- @now, whose resource holds no unexpired lease at @now. Rows another unit
-- is claiming are skipped, not waited for: concurrent claimers take the
-- next items.
SELECT w.seq, w.id, w.kind, w.resource_kind, w.resource_inbox, w.resource_id, w.not_before, w.claims
FROM uma_work w
WHERE w.tenant_id = @tenant_id AND w.kind = ANY (@kinds::text[]) AND w.seq > @after AND NOT w.quarantined
  AND (w.not_before IS NULL OR w.not_before <= @now::timestamptz)
  AND NOT EXISTS (
      SELECT 1 FROM uma_leases l
      WHERE l.tenant_id = w.tenant_id AND l.resource_kind = w.resource_kind
        AND l.resource_inbox IS NOT DISTINCT FROM w.resource_inbox AND l.resource_id = w.resource_id
        AND l.until > @now::timestamptz
  )
ORDER BY w.seq
LIMIT @lim
FOR UPDATE OF w SKIP LOCKED;

-- name: SetWorkFence :exec
UPDATE uma_work
SET fence_owner = @fence_owner::text, fence_generation = @fence_generation::bigint, until = @until::timestamptz,
    claims = claims + 1
WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetWork :one
SELECT kind, resource_kind, resource_inbox, resource_id, fence_owner, fence_generation, quarantined
FROM uma_work
WHERE tenant_id = @tenant_id AND id = @id;

-- name: DeleteWork :exec
DELETE FROM uma_work WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetEvent :one
SELECT body
FROM uma_events
WHERE tenant_id = @tenant_id AND id = @id;
