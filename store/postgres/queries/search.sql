-- Message search index upkeep. Messages written before the search
-- migration hold NULL search_tokens until Migrate fills them in Go.

-- name: ListUnindexedMessages :many
SELECT tenant_id, id, body
FROM uma_messages
WHERE search_tokens IS NULL AND (tenant_id, id) > (@after_tenant::text, @after_id::uuid)
ORDER BY tenant_id, id
LIMIT @lim;

-- name: SetMessageTokens :exec
UPDATE uma_messages SET search_tokens = @search_tokens::text[]
WHERE tenant_id = @tenant_id AND id = @id AND search_tokens IS NULL;

-- name: UnindexedMessagesExist :one
SELECT EXISTS (SELECT 1 FROM uma_messages WHERE search_tokens IS NULL);
