-- Retention purge, work quarantine, claim caps and work depth.

-- name: EventBase :one
SELECT COALESCE((SELECT base FROM uma_event_heads WHERE tenant_id = @tenant_id), 0)::bigint;

-- name: ListEventStamps :many
-- The oldest retained events in position order with their write times.
-- Events at placeholders are not in the log yet.
SELECT position, written_at
FROM uma_events
WHERE tenant_id = @tenant_id AND position > 0
ORDER BY position
LIMIT @lim;

-- name: DeleteEventsThrough :exec
DELETE FROM uma_events WHERE tenant_id = @tenant_id AND position > 0 AND position <= @through;

-- name: SetEventBase :exec
UPDATE uma_event_heads SET base = @base WHERE tenant_id = @tenant_id;

-- name: ListPurgeableOperations :many
-- Operations last written before @before that no work item refers to and
-- whose resource holds no unexpired lease at @now, oldest first.
SELECT o.id, o.written_at
FROM uma_operations o
WHERE o.tenant_id = @tenant_id AND o.written_at < @before
  AND NOT EXISTS (
      SELECT 1 FROM uma_work w
      WHERE w.tenant_id = o.tenant_id AND w.resource_kind = @resource_kind AND w.resource_inbox IS NULL
        AND w.resource_id = o.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM uma_leases l
      WHERE l.tenant_id = o.tenant_id AND l.resource_kind = @resource_kind AND l.resource_inbox IS NULL
        AND l.resource_id = o.id AND l.until > @now::timestamptz
  )
ORDER BY o.written_at, o.id
LIMIT @lim;

-- name: DeleteOperationAttempts :exec
DELETE FROM uma_attempt_evidence
WHERE tenant_id = @tenant_id AND operation_id = ANY ((@ids::text[])::uuid[]);

-- name: DeleteOperations :exec
DELETE FROM uma_operations WHERE tenant_id = @tenant_id AND id = ANY ((@ids::text[])::uuid[]);

-- name: ListPurgeableAttempts :many
-- Attempt evidence written before @before of operations no work item
-- refers to and whose resource holds no unexpired lease at @now, oldest
-- first.
SELECT a.operation_id, a.seq
FROM uma_attempt_evidence a
WHERE a.tenant_id = @tenant_id AND a.written_at < @before
  AND NOT EXISTS (
      SELECT 1 FROM uma_work w
      WHERE w.tenant_id = a.tenant_id AND w.resource_kind = @resource_kind AND w.resource_inbox IS NULL
        AND w.resource_id = a.operation_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM uma_leases l
      WHERE l.tenant_id = a.tenant_id AND l.resource_kind = @resource_kind AND l.resource_inbox IS NULL
        AND l.resource_id = a.operation_id AND l.until > @now::timestamptz
  )
ORDER BY a.written_at, a.seq
LIMIT @lim;

-- name: DeleteAttempts :exec
DELETE FROM uma_attempt_evidence WHERE tenant_id = @tenant_id AND seq = ANY (@seqs::bigint[]);

-- name: ListPurgeableIngress :many
SELECT id
FROM uma_ingress
WHERE tenant_id = @tenant_id AND written_at < @before
ORDER BY written_at, id
LIMIT @lim;

-- name: DeleteIngress :exec
DELETE FROM uma_ingress WHERE tenant_id = @tenant_id AND id = ANY ((@ids::text[])::uuid[]);

-- name: CountClaimedWork :many
-- Unquarantined items of the given kinds holding an unexpired claim at @now.
SELECT kind, count(*)::bigint AS claimed
FROM uma_work
WHERE tenant_id = @tenant_id AND kind = ANY (@kinds::text[]) AND NOT quarantined
  AND fence_owner IS NOT NULL AND until > @now::timestamptz
GROUP BY kind;

-- name: WorkDepth :one
SELECT
    count(*) FILTER (WHERE NOT quarantined AND NOT COALESCE(fence_owner IS NOT NULL AND until > @now::timestamptz, false))::bigint AS pending,
    count(*) FILTER (WHERE NOT quarantined AND COALESCE(fence_owner IS NOT NULL AND until > @now::timestamptz, false))::bigint AS claimed,
    count(*) FILTER (WHERE quarantined)::bigint AS quarantined
FROM uma_work
WHERE tenant_id = @tenant_id AND kind = @kind;

-- name: QuarantineWork :exec
UPDATE uma_work SET quarantined = true WHERE tenant_id = @tenant_id AND id = @id;

-- name: RequeueWork :execrows
UPDATE uma_work
SET quarantined = false, claims = 0, fence_owner = NULL, fence_generation = NULL, until = NULL, not_before = NULL
WHERE tenant_id = @tenant_id AND id = @id AND quarantined;

-- name: ListQuarantinedWork :many
SELECT id, kind, not_before, fence_owner, fence_generation, until, claims
FROM uma_work
WHERE tenant_id = @tenant_id AND resource_kind = @resource_kind
  AND resource_inbox IS NOT DISTINCT FROM sqlc.narg(resource_inbox)::uuid AND resource_id = @resource_id
  AND quarantined
ORDER BY seq;

-- name: ListPurgeableContacts :many
-- Contacts no identity points at whose latest link record leaving them was
-- written before @before, oldest such record first.
SELECT l.from_contact
FROM uma_identity_links l
JOIN uma_contacts c ON c.tenant_id = l.tenant_id AND c.id = l.from_contact
WHERE l.tenant_id = @tenant_id
  AND NOT EXISTS (
      SELECT 1 FROM uma_identities i WHERE i.tenant_id = l.tenant_id AND i.contact_id = l.from_contact
  )
GROUP BY l.from_contact
HAVING max(l.written_at) < @before::timestamptz
ORDER BY max(l.written_at), l.from_contact
LIMIT @lim;

-- name: DeleteContacts :exec
DELETE FROM uma_contacts WHERE tenant_id = @tenant_id AND id = ANY ((@ids::text[])::uuid[]);
