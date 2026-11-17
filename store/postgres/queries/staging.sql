-- Attachment staging intents.

-- name: InsertStaging :execrows
INSERT INTO uma_blob_staging (tenant_id, blob_id, inbox_id, attachment_id, export_id, written_at)
VALUES (@tenant_id, @blob_id, sqlc.narg(inbox_id)::uuid, sqlc.narg(attachment_id)::uuid, sqlc.narg(export_id)::uuid, @written_at)
ON CONFLICT DO NOTHING;

-- name: GetStaging :one
SELECT inbox_id, attachment_id, export_id, abandoned
FROM uma_blob_staging
WHERE tenant_id = @tenant_id AND blob_id = @blob_id;

-- name: BindStaging :execrows
DELETE FROM uma_blob_staging WHERE tenant_id = @tenant_id AND blob_id = @blob_id AND NOT abandoned;

-- name: DeleteStaging :exec
DELETE FROM uma_blob_staging WHERE tenant_id = @tenant_id AND blob_id = @blob_id;

-- name: ListStagingSweep :many
-- Intents abandoned already or recorded before @before, in recording order.
SELECT blob_id, inbox_id, attachment_id, export_id
FROM uma_blob_staging
WHERE tenant_id = @tenant_id AND (abandoned OR written_at < @before)
ORDER BY seq
LIMIT @lim;

-- name: AbandonStaging :exec
UPDATE uma_blob_staging SET abandoned = true
WHERE tenant_id = @tenant_id AND blob_id = ANY ((@ids::text[])::uuid[]);
