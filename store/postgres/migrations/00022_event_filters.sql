-- Event log filters: Reader.Events selects by conversation and by
-- Filter.Since (on Event.RecordedAt) without scanning the log.
--
--   - conversation_id is the conversation the event is about, resolved
--     from the event exactly as store.EventConversation does: the resource
--     when it is a conversation, else the conversation a builtin version 1
--     payload names at its documented path, kept only when it is a valid
--     reference in the event's own tenant and inbox. NULL for an event
--     with none. AppendEvents writes it; an event never changes.
--   - recorded_unix and recorded_nanos are the event's RecordedAt as Unix
--     seconds and the nanosecond within the second, exact like the message
--     columns of 00014, so Since compares as Go's time comparison does.
--   - recorded_max_unix and recorded_max_nanos are the running maximum of
--     RecordedAt over the tenant's log up to and including the event. A
--     unit sets RecordedAt before it commits, so RecordedAt alone does not
--     follow position; the running maximum does, by construction. A read
--     with Since starts after the last position whose running maximum is
--     still below Since (no earlier event can match) and compares each
--     event's own RecordedAt from there. uma_event_heads keeps the maximum
--     of every event ever appended to the tenant, purged ones included, and
--     AppendEvents raises it under the head row lock it already holds.
--   - uma_events_conversation serves a conversation read as a range scan
--     in position order; uma_events_recorded_max finds the Since start.
--   - Rows written before this migration are backfilled from their bodies
--     with the same rules; a body without recorded_at reads as the zero
--     time, as Go decodes it.

-- +goose Up

ALTER TABLE uma_events
    ADD COLUMN conversation_id    uuid,
    ADD COLUMN recorded_unix      bigint NOT NULL DEFAULT 0,
    ADD COLUMN recorded_nanos     integer NOT NULL DEFAULT 0 CHECK (recorded_nanos BETWEEN 0 AND 999999999),
    ADD COLUMN recorded_max_unix  bigint NOT NULL DEFAULT 0,
    ADD COLUMN recorded_max_nanos integer NOT NULL DEFAULT 0 CHECK (recorded_max_nanos BETWEEN 0 AND 999999999),
    ADD CONSTRAINT uma_events_conversation_scope CHECK (conversation_id IS NULL OR inbox_id IS NOT NULL);

ALTER TABLE uma_event_heads
    ADD COLUMN recorded_max_unix  bigint NOT NULL DEFAULT -62135596800,
    ADD COLUMN recorded_max_nanos integer NOT NULL DEFAULT 0 CHECK (recorded_max_nanos BETWEEN 0 AND 999999999);

UPDATE uma_events SET
    recorded_unix = extract(epoch FROM regexp_replace(coalesce(body->>'recorded_at', '0001-01-01T00:00:00Z'), '\.[0-9]+', '')::timestamptz)::bigint,
    recorded_nanos = coalesce(rpad(substring(body->>'recorded_at' FROM '\.([0-9]+)'), 9, '0'), '0')::integer;

UPDATE uma_events e SET recorded_max_unix = floor(m.v / 1000000000), recorded_max_nanos = m.v - floor(m.v / 1000000000) * 1000000000
FROM (
    SELECT tenant_id, position,
        max(recorded_unix::numeric * 1000000000 + recorded_nanos) OVER (PARTITION BY tenant_id ORDER BY position) AS v
    FROM uma_events
) m
WHERE e.tenant_id = m.tenant_id AND e.position = m.position;

UPDATE uma_event_heads h SET recorded_max_unix = l.recorded_max_unix, recorded_max_nanos = l.recorded_max_nanos
FROM (
    SELECT DISTINCT ON (tenant_id) tenant_id, recorded_max_unix, recorded_max_nanos
    FROM uma_events
    ORDER BY tenant_id, position DESC
) l
WHERE h.tenant_id = l.tenant_id;

UPDATE uma_events e SET conversation_id = c.id::uuid
FROM (
    SELECT tenant_id, position,
        CASE WHEN body #>> '{resource,kind}' = 'conversation' THEN body #>> '{resource,id}'
             WHEN body #>> '{payload,version}' = '1' THEN (
                 SELECT CASE WHEN json_typeof(ref #> '{inbox,tenant}') = 'string' AND ref #>> '{inbox,tenant}' = tenant_id
                              AND json_typeof(ref #> '{inbox,id}') = 'string' AND ref #>> '{inbox,id}' = inbox_id::text
                              AND json_typeof(ref #> '{id}') = 'string'
                             THEN ref #>> '{id}' END
                 FROM (SELECT CASE
                     WHEN body #>> '{payload,name}' IN ('uma.message.received', 'uma.message.created', 'uma.message.updated')
                         THEN body #> '{payload,data,message,conversation}'
                     WHEN body #>> '{payload,name}' IN ('uma.conversation.changed',
                             'uma.conversation.label_added', 'uma.conversation.label_removed',
                             'uma.conversation.assigned', 'uma.conversation.unassigned',
                             'uma.conversation.snoozed', 'uma.conversation.unsnoozed',
                             'uma.conversation.linked', 'uma.conversation.unlinked')
                         THEN body #> '{payload,data,conversation}'
                     WHEN body #>> '{payload,name}' = 'uma.task.updated'
                         THEN body #> '{payload,data,task,conversation}'
                     WHEN body #>> '{payload,name}' = 'uma.work_item.updated'
                         THEN body #> '{payload,data,work_item,conversation}'
                     WHEN body #>> '{payload,name}' IN ('uma.interaction.received', 'uma.interaction.updated')
                         THEN body #> '{payload,data,interaction,conversation}'
                 END AS ref) r
             ) END AS id
    FROM uma_events
    WHERE inbox_id IS NOT NULL
) c
WHERE c.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  AND e.tenant_id = c.tenant_id AND e.position = c.position;

ALTER TABLE uma_events ALTER COLUMN recorded_unix DROP DEFAULT;
ALTER TABLE uma_events ALTER COLUMN recorded_nanos DROP DEFAULT;
ALTER TABLE uma_events ALTER COLUMN recorded_max_unix DROP DEFAULT;
ALTER TABLE uma_events ALTER COLUMN recorded_max_nanos DROP DEFAULT;

CREATE INDEX uma_events_conversation ON uma_events (tenant_id, inbox_id, conversation_id, position)
    WHERE conversation_id IS NOT NULL;
CREATE INDEX uma_events_recorded_max ON uma_events (tenant_id, recorded_max_unix, recorded_max_nanos, position);

-- +goose Down

DROP INDEX uma_events_recorded_max;
DROP INDEX uma_events_conversation;
ALTER TABLE uma_event_heads
    DROP COLUMN recorded_max_nanos,
    DROP COLUMN recorded_max_unix;
ALTER TABLE uma_events
    DROP CONSTRAINT uma_events_conversation_scope,
    DROP COLUMN recorded_max_nanos,
    DROP COLUMN recorded_max_unix,
    DROP COLUMN recorded_nanos,
    DROP COLUMN recorded_unix,
    DROP COLUMN conversation_id;
