-- Privacy: the records holding copies of a message's content, and export
-- records.

-- name: ListIngressByMessage :many
-- Ingress records on one channel naming a provider message ID, in id order.
SELECT body
FROM uma_ingress
WHERE message_id = @message_id AND tenant_id = @tenant_id AND channel_id = @channel_id
ORDER BY id;

-- name: OperationBusy :one
-- Whether a work item refers to the operation or its resource holds an
-- unexpired lease at @now.
SELECT EXISTS (
    SELECT 1 FROM uma_work w
    WHERE w.tenant_id = @tenant_id AND w.resource_kind = @resource_kind AND w.resource_inbox IS NULL
      AND w.resource_id = @id
    UNION ALL
    SELECT 1 FROM uma_leases l
    WHERE l.tenant_id = @tenant_id AND l.resource_kind = @resource_kind AND l.resource_inbox IS NULL
      AND l.resource_id = @id AND l.until > @now::timestamptz
);

-- name: RedactOperation :exec
-- Replaces an operation's stored body without touching its state,
-- revision or write time.
UPDATE uma_operations SET body = @body WHERE tenant_id = @tenant_id AND id = @id;

-- Export records.

-- name: GetExport :one
SELECT body, revision FROM uma_exports WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertExport :exec
INSERT INTO uma_exports (tenant_id, id, body, revision, written_at)
VALUES (@tenant_id, @id, @body, @revision, @written_at);

-- name: UpdateExport :execrows
UPDATE uma_exports SET body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: ListPurgeableExports :many
-- Exports first written before @before, oldest first.
SELECT id, body
FROM uma_exports
WHERE tenant_id = @tenant_id AND written_at < @before
ORDER BY written_at, id
LIMIT @lim;

-- name: DeleteExports :exec
DELETE FROM uma_exports WHERE tenant_id = @tenant_id AND id = ANY ((@ids::text[])::uuid[]);
