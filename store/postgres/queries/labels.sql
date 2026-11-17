-- Label definitions and the label rows of conversations. A label's
-- name_key is its name's model.LabelNameKey, computed by the store.

-- name: GetLabel :one
SELECT body, revision FROM uma_labels WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetLabelByNameKey :one
SELECT id, body FROM uma_labels WHERE tenant_id = @tenant_id AND name_key = @name_key;

-- name: InsertLabel :exec
INSERT INTO uma_labels (tenant_id, id, name_key, body, revision)
VALUES (@tenant_id, @id, @name_key, @body, @revision);

-- name: UpdateLabel :execrows
UPDATE uma_labels
SET name_key = @name_key, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: DeleteLabel :execrows
DELETE FROM uma_labels WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: ListLabels :many
SELECT pos, body
FROM uma_labels
WHERE tenant_id = @tenant_id AND pos > @after
ORDER BY pos
LIMIT @lim;

-- name: CountLabels :one
SELECT count(*) FROM uma_labels WHERE tenant_id = @tenant_id AND id = ANY (@ids::uuid[]);

-- name: LabelAssigned :one
SELECT EXISTS (SELECT 1 FROM uma_conversation_labels WHERE tenant_id = @tenant_id AND label_id = @label_id);

-- name: DeleteConversationLabels :exec
DELETE FROM uma_conversation_labels WHERE tenant_id = @tenant_id AND conversation_id = @conversation_id;

-- name: InsertConversationLabels :exec
INSERT INTO uma_conversation_labels (tenant_id, conversation_id, label_id)
SELECT @tenant_id, @conversation_id, unnest(@label_ids::uuid[]);
