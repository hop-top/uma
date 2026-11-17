-- Conversation links and read markers.
--
--   - link_count is the number of links a conversation holds (its body's
--     links array), rewritten from the record on every conversation write;
--     Reader.Conversations evaluates Filter.Linked on it. Rows written
--     before this migration are backfilled from their bodies.
--   - uma_read_markers holds one row per (conversation, actor): the read
--     marker body, its revision and read_through as exact (Unix seconds,
--     nanoseconds). Writing one never touches the conversation row.
--   - Filter.Unread compares each message's recorded_* with read_through,
--     so every message row now carries recorded_*: the rows migration 20
--     left NULL are backfilled from their bodies, rounded to the
--     microsecond as PostgreSQL parses timestamps.

-- +goose Up

ALTER TABLE uma_conversations ADD COLUMN link_count integer NOT NULL DEFAULT 0;

UPDATE uma_conversations SET link_count = json_array_length(body -> 'links')
WHERE json_typeof(body -> 'links') = 'array';

CREATE TABLE uma_read_markers (
    tenant_id          text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id           uuid NOT NULL,
    conversation_id    uuid NOT NULL,
    actor              text COLLATE "C" NOT NULL CHECK (actor <> ''),
    read_through_unix  bigint NOT NULL,
    read_through_nanos integer NOT NULL,
    body               json NOT NULL,
    revision           text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, conversation_id, actor),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id)
);

UPDATE uma_messages SET
    recorded_unix  = floor(extract(epoch FROM (body ->> 'recorded_at')::timestamptz))::bigint,
    recorded_nanos = (extract(microseconds FROM (body ->> 'recorded_at')::timestamptz)::bigint % 1000000 * 1000)::integer
WHERE recorded_unix IS NULL;

-- +goose Down

DROP TABLE uma_read_markers;

ALTER TABLE uma_conversations DROP COLUMN link_count;
