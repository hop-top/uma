-- Agent task lookup, status order and task message history.
--
--   - uma_tasks holds the task's External reference as explicit columns,
--     all NULL when the task has none. A unique index keeps a reference on
--     one task per inbox (Reader.TaskByReference); NULLs never collide. The
--     channel's tenant is the row's: the store scopes references.
--   - status_unix and status_nanos are the task's StatusAt as Unix seconds
--     and the nanosecond within the second, exact like uma_conversations
--     created_unix. Tasks lists most recent status first, then ID
--     descending, from indexes ending in that order: tenant-wide, per
--     conversation and per owner. The pos indexes go: no read pages tasks
--     by pos any more, and the conversation index serves the foreign key.
--   - owner_inbox_id and owner_id copy the conversation's Owner (in
--     per-agent-context mode, the principal whose request created it), so
--     a principal's tasks list from one index. PutConversation keeps them
--     in step.
--   - uma_messages task_id and in_reply_to_id are the message's task and
--     parent, both in the message's own conversation, indexed so a task's
--     message history and the replies to a message are found by key.
--   - Rows written before this migration are backfilled from their
--     bodies; a task or parent in another conversation is not indexed.

-- +goose Up

ALTER TABLE uma_tasks
    ADD COLUMN external_channel_id uuid,
    ADD COLUMN external_namespace  text COLLATE "C",
    ADD COLUMN external_kind       text COLLATE "C",
    ADD COLUMN external_id         text COLLATE "C",
    ADD COLUMN external_parent_id  text COLLATE "C",
    ADD COLUMN status_unix         bigint NOT NULL DEFAULT 0,
    ADD COLUMN status_nanos        integer NOT NULL DEFAULT 0 CHECK (status_nanos BETWEEN 0 AND 999999999),
    ADD COLUMN owner_inbox_id      uuid,
    ADD COLUMN owner_id            uuid,
    ADD CONSTRAINT uma_tasks_external_whole CHECK (
        (external_channel_id IS NULL) = (external_namespace IS NULL)
        AND (external_channel_id IS NULL) = (external_kind IS NULL)
        AND (external_channel_id IS NULL) = (external_id IS NULL)
        AND (external_channel_id IS NULL) = (external_parent_id IS NULL)),
    ADD CONSTRAINT uma_tasks_owner_whole CHECK ((owner_inbox_id IS NULL) = (owner_id IS NULL));

UPDATE uma_tasks SET
    status_unix = extract(epoch FROM regexp_replace(coalesce(body->>'status_at', '0001-01-01T00:00:00Z'), '\.[0-9]+', '')::timestamptz)::bigint,
    status_nanos = coalesce(rpad(substring(body->>'status_at' FROM '\.([0-9]+)'), 9, '0'), '0')::integer;
UPDATE uma_tasks SET
    external_channel_id = (body->'external'->'channel'->>'id')::uuid,
    external_namespace = coalesce(body->'external'->>'namespace', ''),
    external_kind = coalesce(body->'external'->>'kind', ''),
    external_id = coalesce(body->'external'->>'id', ''),
    external_parent_id = coalesce(body->'external'->>'parent_id', '')
WHERE body->'external'->'channel'->>'id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
UPDATE uma_tasks t SET
    owner_inbox_id = (c.body->'owner'->'inbox'->>'id')::uuid,
    owner_id = (c.body->'owner'->>'id')::uuid
FROM uma_conversations c
WHERE c.tenant_id = t.tenant_id AND c.inbox_id = t.inbox_id AND c.id = t.conversation_id
  AND c.body->'owner'->'inbox'->>'id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  AND c.body->'owner'->>'id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
ALTER TABLE uma_tasks ALTER COLUMN status_unix DROP DEFAULT;
ALTER TABLE uma_tasks ALTER COLUMN status_nanos DROP DEFAULT;

DROP INDEX uma_tasks_pos;
DROP INDEX uma_tasks_conversation;
CREATE UNIQUE INDEX uma_tasks_external ON uma_tasks
    (tenant_id, inbox_id, external_channel_id, external_namespace, external_kind, external_id, external_parent_id);
CREATE INDEX uma_tasks_status ON uma_tasks (tenant_id, status_unix DESC, status_nanos DESC, id DESC);
CREATE INDEX uma_tasks_conversation_status ON uma_tasks
    (tenant_id, inbox_id, conversation_id, status_unix DESC, status_nanos DESC, id DESC);
CREATE INDEX uma_tasks_owner_status ON uma_tasks
    (tenant_id, owner_inbox_id, owner_id, status_unix DESC, status_nanos DESC, id DESC)
    WHERE owner_id IS NOT NULL;

ALTER TABLE uma_messages ADD COLUMN task_id uuid, ADD COLUMN in_reply_to_id uuid;
UPDATE uma_messages SET task_id = (body->'task'->>'id')::uuid
WHERE body->'task'->'conversation'->>'id' = conversation_id::text
  AND body->'task'->'conversation'->'inbox'->>'id' = inbox_id::text
  AND body->'task'->'conversation'->'inbox'->>'tenant' = tenant_id
  AND body->'task'->>'id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
UPDATE uma_messages SET in_reply_to_id = (body->'in_reply_to'->>'id')::uuid
WHERE body->'in_reply_to'->'conversation'->>'id' = conversation_id::text
  AND body->'in_reply_to'->'conversation'->'inbox'->>'id' = inbox_id::text
  AND body->'in_reply_to'->'conversation'->'inbox'->>'tenant' = tenant_id
  AND body->'in_reply_to'->>'id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
CREATE INDEX uma_messages_task ON uma_messages (tenant_id, task_id, pos) WHERE task_id IS NOT NULL;
CREATE INDEX uma_messages_in_reply_to ON uma_messages (tenant_id, in_reply_to_id, pos) WHERE in_reply_to_id IS NOT NULL;

-- +goose Down

DROP INDEX uma_messages_in_reply_to;
DROP INDEX uma_messages_task;
ALTER TABLE uma_messages DROP COLUMN in_reply_to_id, DROP COLUMN task_id;

DROP INDEX uma_tasks_owner_status;
DROP INDEX uma_tasks_conversation_status;
DROP INDEX uma_tasks_status;
DROP INDEX uma_tasks_external;
CREATE INDEX uma_tasks_pos ON uma_tasks (tenant_id, pos);
CREATE INDEX uma_tasks_conversation ON uma_tasks (tenant_id, inbox_id, conversation_id, pos);
ALTER TABLE uma_tasks
    DROP CONSTRAINT uma_tasks_owner_whole,
    DROP CONSTRAINT uma_tasks_external_whole,
    DROP COLUMN owner_id,
    DROP COLUMN owner_inbox_id,
    DROP COLUMN status_nanos,
    DROP COLUMN status_unix,
    DROP COLUMN external_parent_id,
    DROP COLUMN external_id,
    DROP COLUMN external_kind,
    DROP COLUMN external_namespace,
    DROP COLUMN external_channel_id;
