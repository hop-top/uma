-- Atomic claims: identities and their link history, conversation creation
-- claims and ingress deduplication.

-- name: GetIdentityByKey :one
SELECT id, body, revision, link_head
FROM uma_identities
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND namespace = @namespace AND source_id = @source_id;

-- name: GetIdentity :one
SELECT body, revision, link_head
FROM uma_identities
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id;

-- name: IdentityIDExists :one
SELECT EXISTS (SELECT 1 FROM uma_identities WHERE tenant_id = @tenant_id AND id = @id);

-- name: InsertIdentity :execrows
-- Inserts the identity unless its key or ID is held in the unit's
-- snapshot: no row is inserted then. No lookup precedes it, so it leaves
-- no predicate lock for concurrent claims' inserts to conflict with; a key
-- or ID a concurrent unit committed fails this unit as a serialization
-- failure.
INSERT INTO uma_identities (tenant_id, inbox_id, id, namespace, source_id, contact_id, body, revision)
VALUES (@tenant_id, @inbox_id, @id, @namespace, @source_id, @contact_id, @body, @revision)
ON CONFLICT DO NOTHING;

-- name: LinkIdentity :execrows
UPDATE uma_identities
SET contact_id = @contact_id, link_head = @link_head, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id AND revision = @expected;

-- name: ListIdentities :many
SELECT pos, body
FROM uma_identities
WHERE tenant_id = @tenant_id AND pos > @after
  AND (cardinality(@inboxes::text[]) = 0 OR inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (sqlc.narg(contact_id)::uuid IS NULL OR contact_id = sqlc.narg(contact_id)::uuid)
ORDER BY pos
LIMIT @lim;

-- name: LinkIDExists :one
SELECT EXISTS (SELECT 1 FROM uma_identity_links WHERE tenant_id = @tenant_id AND id = @id);

-- name: EvidenceUsed :one
SELECT EXISTS (
    SELECT 1 FROM uma_identity_links
    WHERE tenant_id = @tenant_id AND evidence_verifier = @verifier AND evidence_reference = @reference
);

-- name: InsertLink :exec
INSERT INTO uma_identity_links (
    tenant_id, id, inbox_id, identity_id, from_contact, to_contact, supersedes,
    evidence_verifier, evidence_reference, body, written_at
) VALUES (
    @tenant_id, @id, @inbox_id, @identity_id, @from_contact, @to_contact, @supersedes,
    @evidence_verifier, @evidence_reference, @body, @written_at
);

-- name: ListLinks :many
SELECT seq, body
FROM uma_identity_links
WHERE tenant_id = @tenant_id AND seq > @after
  AND (sqlc.narg(identity_inbox)::uuid IS NULL
       OR (inbox_id = sqlc.narg(identity_inbox)::uuid AND identity_id = sqlc.narg(identity_id)::uuid))
  AND (sqlc.narg(contact_id)::uuid IS NULL
       OR from_contact = sqlc.narg(contact_id)::uuid OR to_contact = sqlc.narg(contact_id)::uuid)
ORDER BY seq
LIMIT @lim;

-- name: ListIdentityLinkBodies :many
-- Every link record of one identity, in append order.
SELECT id, body
FROM uma_identity_links
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND identity_id = @identity_id
ORDER BY seq;

-- name: SetLinkBody :exec
UPDATE uma_identity_links SET body = @body WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetConversationClaim :one
SELECT c.body
FROM uma_conversation_claims k
JOIN uma_conversations c ON c.tenant_id = k.tenant_id AND c.inbox_id = k.inbox_id AND c.id = k.conversation_id
WHERE k.tenant_id = @tenant_id AND k.inbox_id = @inbox_id AND k.mode = @mode
  AND k.selection_key = @selection_key AND k.create_key = @create_key;

-- name: ConversationIDExists :one
SELECT EXISTS (SELECT 1 FROM uma_conversations WHERE tenant_id = @tenant_id AND id = @id);

-- name: InsertConversationClaim :execrows
-- Inserts the claim unless its selection and create key are held in the
-- unit's snapshot: no row is inserted then, as InsertIdentity.
INSERT INTO uma_conversation_claims (tenant_id, inbox_id, mode, selection_key, create_key, conversation_id)
VALUES (@tenant_id, @inbox_id, @mode, @selection_key, @create_key, @conversation_id)
ON CONFLICT DO NOTHING;

-- name: DeleteNewConversation :exec
-- Removes a conversation the unit inserted for a claim it then found held;
-- nothing references it yet.
DELETE FROM uma_conversations WHERE tenant_id = @tenant_id AND id = @id;

-- name: Candidates :many
-- The group's members that are not closed. The group's claims are read
-- first and the conversations they name by key: a planner left to choose
-- may read the inbox's conversations and probe the claims for each,
-- leaving a predicate lock on every page of the inbox's conversations that
-- each new conversation of the inbox lands on. A claim's conversation is
-- in the claim's inbox (foreign key), so the join needs no inbox test.
WITH members AS MATERIALIZED (
    SELECT k.conversation_id
    FROM uma_conversation_claims k
    WHERE k.tenant_id = @tenant_id AND k.inbox_id = @inbox_id AND k.mode = @mode AND k.selection_key = @selection_key
)
SELECT c.body
FROM members m
JOIN uma_conversations c ON c.tenant_id = @tenant_id AND c.id = m.conversation_id
WHERE c.status <> @closed
LIMIT @lim;

-- name: NewestClosedCandidate :many
-- The group's newest closed member by CreatedAt, then ID: canonical
-- lowercase UUID text orders as the uuid bytes do. Read as Candidates.
WITH members AS MATERIALIZED (
    SELECT k.conversation_id
    FROM uma_conversation_claims k
    WHERE k.tenant_id = @tenant_id AND k.inbox_id = @inbox_id AND k.mode = @mode AND k.selection_key = @selection_key
)
SELECT c.body
FROM members m
JOIN uma_conversations c ON c.tenant_id = @tenant_id AND c.id = m.conversation_id
WHERE c.status = @closed
ORDER BY c.created_unix DESC, c.created_nanos DESC, c.id DESC
LIMIT 1;

-- name: GetIngressByDedup :one
SELECT body FROM uma_ingress
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND dedup_key = @dedup_key;

-- name: GetIngress :one
SELECT body FROM uma_ingress WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertIngress :execrows
-- Inserts the record unless its dedup key or ID is held in the unit's
-- snapshot: no row is inserted then, as InsertIdentity.
INSERT INTO uma_ingress (tenant_id, id, channel_id, dedup_key, message_id, body, written_at)
VALUES (@tenant_id, @id, @channel_id, @dedup_key, @message_id, @body, @written_at)
ON CONFLICT DO NOTHING;

-- name: UpdateIngress :exec
UPDATE uma_ingress SET body = @body WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetMessageClaim :one
SELECT m.body
FROM uma_message_claims k
JOIN uma_messages m ON m.tenant_id = k.tenant_id AND m.id = k.message_id
WHERE k.tenant_id = @tenant_id AND k.inbox_id = @inbox_id
  AND k.channel_tenant = @channel_tenant AND k.channel_id = @channel_id
  AND k.namespace = @namespace AND k.kind = @kind AND k.ext_id = @ext_id AND k.parent_id = @parent_id;

-- name: InsertMessageClaim :execrows
-- Inserts the claim unless its reference is held in the unit's snapshot:
-- no row is inserted then, as InsertIdentity.
INSERT INTO uma_message_claims (tenant_id, inbox_id, channel_tenant, channel_id, namespace, kind, ext_id, parent_id, message_id)
VALUES (@tenant_id, @inbox_id, @channel_tenant, @channel_id, @namespace, @kind, @ext_id, @parent_id, @message_id)
ON CONFLICT DO NOTHING;

-- name: DeleteNewMessage :exec
-- Removes a message the unit inserted for a claim it then found held, and
-- with it its reference and attachment rows.
DELETE FROM uma_messages WHERE tenant_id = @tenant_id AND id = @id;
