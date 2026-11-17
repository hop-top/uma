-- Interaction records. response_token is selected by GetInteractionToken
-- only: no other query reads it, and the audit in
-- interaction_token_test.go fails when one does.

-- name: GetInteraction :one
SELECT body, revision, ingress_id FROM uma_interactions
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id;

-- name: GetInteractionToken :one
SELECT response_token, token_expires_unix, token_expires_nanos FROM uma_interactions
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id;

-- name: InsertInteraction :exec
INSERT INTO uma_interactions (tenant_id, inbox_id, conversation_id, id, actor_inbox_id, actor_id, ingress_id,
                              body, revision, response_token, token_expires_unix, token_expires_nanos,
                              message_channel_tenant, message_channel_id, message_namespace, message_kind,
                              message_ext_id, message_parent_id, message_linked)
VALUES (@tenant_id, @inbox_id, @conversation_id, @id, @actor_inbox_id, @actor_id, @ingress_id,
        @body, @revision, @response_token, @token_expires_unix, @token_expires_nanos,
        sqlc.narg(message_channel_tenant), sqlc.narg(message_channel_id), sqlc.narg(message_namespace),
        sqlc.narg(message_kind), sqlc.narg(message_ext_id), sqlc.narg(message_parent_id), @message_linked);

-- name: UpdateInteraction :execrows
-- A response claim: the body and revision change, the token does not.
UPDATE uma_interactions
SET body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: LinkInteraction :execrows
-- A message link: the body and revision change and the row leaves the
-- unlinked index; the token does not change.
UPDATE uma_interactions
SET body = @body, revision = @revision, message_linked = true
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: ListUnlinkedInteractions :many
-- The predicate is the partial index uma_interactions_unlinked's
-- (migration 00025); every other field of the provider message is
-- compared exactly.
SELECT pos, body
FROM uma_interactions
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND message_ext_id = @ext_id::text AND NOT message_linked
  AND message_channel_tenant = @channel_tenant::text AND message_channel_id = @channel_id::text
  AND message_namespace = @namespace::text AND message_kind = @kind::text AND message_parent_id = @parent_id::text
  AND pos > @after
ORDER BY pos
LIMIT @lim;

-- name: RedactInteraction :execrows
-- The tombstone replaces the body and revision and drops the token.
UPDATE uma_interactions
SET body = @body, revision = @revision, response_token = ''
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: ListInteractions :many
SELECT pos, body
FROM uma_interactions
WHERE tenant_id = @tenant_id AND pos > @after
  AND (cardinality(@inboxes::text[]) = 0 OR inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (sqlc.narg(conversation_inbox)::uuid IS NULL
       OR (inbox_id = sqlc.narg(conversation_inbox)::uuid AND conversation_id = sqlc.narg(conversation_id)::uuid))
  AND (sqlc.narg(actor_inbox)::uuid IS NULL
       OR (actor_inbox_id = sqlc.narg(actor_inbox)::uuid AND actor_id = sqlc.narg(actor_id)::uuid))
ORDER BY pos
LIMIT @lim;

-- name: ClearExpiredInteractionTokens :execrows
-- Clears the tokens of the first @lim interactions, in recording order,
-- whose token expired before the given instant. Rows keep everything else.
UPDATE uma_interactions i
SET response_token = ''
FROM (
    SELECT d.tenant_id, d.inbox_id, d.conversation_id, d.id
    FROM uma_interactions d
    WHERE d.tenant_id = @tenant_id AND d.response_token <> ''
      AND (d.token_expires_unix, d.token_expires_nanos) < (@before_unix::bigint, @before_nanos::integer)
    ORDER BY d.pos
    LIMIT @lim
) due
WHERE i.tenant_id = due.tenant_id AND i.inbox_id = due.inbox_id
  AND i.conversation_id = due.conversation_id AND i.id = due.id;

-- name: ExpiredInteractionTokensRemain :one
SELECT EXISTS (
    SELECT 1 FROM uma_interactions
    WHERE tenant_id = @tenant_id AND response_token <> ''
      AND (token_expires_unix, token_expires_nanos) < (@before_unix::bigint, @before_nanos::integer)
) AS remain;
