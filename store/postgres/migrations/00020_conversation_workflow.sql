-- Conversation workflow state: Reader.Conversations honours Filter.Assignee
-- and Filter.Snoozed.
--
--   - assignee is the conversation's Assignee, NULL when unassigned.
--   - snooze_until_* and snooze_at_* are its Snooze.Until and Snooze.At as
--     (Unix seconds, nanoseconds), exact like created_*; all four are NULL
--     without a snooze. PutConversation rewrites every column from the
--     record it stores, so they cannot drift from the body.
--   - recorded_* on uma_messages is the message's RecordedAt, exact, written
--     by every message write. A snooze is in effect while snooze_until is
--     after the store clock and no live inbound message (the predicate of
--     uma_messages_inbound) has recorded_* after snooze_at; that partial
--     index serves the lookup per conversation.
--   - Rows written before this migration are backfilled from their bodies,
--     rounded to the microsecond: conversations that hold an assignee or a
--     snooze, and the messages of conversations that hold a snooze. Other
--     messages keep recorded_* NULL: no snooze existed when they were
--     recorded, and a NULL never compares after snooze_at.

-- +goose Up

ALTER TABLE uma_conversations
    ADD COLUMN assignee           text,
    ADD COLUMN snooze_until_unix  bigint,
    ADD COLUMN snooze_until_nanos integer,
    ADD COLUMN snooze_at_unix     bigint,
    ADD COLUMN snooze_at_nanos    integer;

ALTER TABLE uma_messages
    ADD COLUMN recorded_unix  bigint,
    ADD COLUMN recorded_nanos integer;

UPDATE uma_conversations SET assignee = body ->> 'assignee'
WHERE body ->> 'assignee' IS NOT NULL;

UPDATE uma_conversations SET
    snooze_until_unix  = floor(extract(epoch FROM (body #>> '{snooze,until}')::timestamptz))::bigint,
    snooze_until_nanos = (extract(microseconds FROM (body #>> '{snooze,until}')::timestamptz)::bigint % 1000000 * 1000)::integer,
    snooze_at_unix     = floor(extract(epoch FROM (body #>> '{snooze,at}')::timestamptz))::bigint,
    snooze_at_nanos    = (extract(microseconds FROM (body #>> '{snooze,at}')::timestamptz)::bigint % 1000000 * 1000)::integer
WHERE json_typeof(body -> 'snooze') = 'object';

UPDATE uma_messages m SET
    recorded_unix  = floor(extract(epoch FROM (m.body ->> 'recorded_at')::timestamptz))::bigint,
    recorded_nanos = (extract(microseconds FROM (m.body ->> 'recorded_at')::timestamptz)::bigint % 1000000 * 1000)::integer
FROM uma_conversations c
WHERE c.tenant_id = m.tenant_id AND c.inbox_id = m.inbox_id AND c.id = m.conversation_id
  AND c.snooze_at_unix IS NOT NULL;

-- +goose Down

ALTER TABLE uma_messages
    DROP COLUMN recorded_nanos,
    DROP COLUMN recorded_unix;

ALTER TABLE uma_conversations
    DROP COLUMN snooze_at_nanos,
    DROP COLUMN snooze_at_unix,
    DROP COLUMN snooze_until_nanos,
    DROP COLUMN snooze_until_unix,
    DROP COLUMN assignee;
