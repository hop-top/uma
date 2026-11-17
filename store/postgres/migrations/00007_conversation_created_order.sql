-- Conversation creation order for candidate selection.
--
--   - created_unix and created_nanos are the conversation's CreatedAt as Unix
--     seconds and the nanosecond within the second: exact, so the store
--     orders conversations as Go's time comparison does. timestamptz keeps
--     microseconds only.
--   - Candidates reads a group's members that are not closed plus its newest
--     closed member. The group is found through the claims primary key, and
--     the newest closed member is ordered over the group's rows, so no new
--     index is needed.
--   - Rows written before this migration are backfilled from the stored
--     body's created_at, RFC 3339 text with up to nine fraction digits.

-- +goose Up

ALTER TABLE uma_conversations ADD COLUMN created_unix bigint NOT NULL DEFAULT 0;
ALTER TABLE uma_conversations ADD COLUMN created_nanos integer NOT NULL DEFAULT 0
    CHECK (created_nanos BETWEEN 0 AND 999999999);
UPDATE uma_conversations SET
    created_unix = extract(epoch FROM regexp_replace(body->>'created_at', '\.[0-9]+', '')::timestamptz)::bigint,
    created_nanos = coalesce(rpad(substring(body->>'created_at' FROM '\.([0-9]+)'), 9, '0'), '0')::integer;
ALTER TABLE uma_conversations ALTER COLUMN created_unix DROP DEFAULT;
ALTER TABLE uma_conversations ALTER COLUMN created_nanos DROP DEFAULT;

-- +goose Down

ALTER TABLE uma_conversations DROP COLUMN created_nanos;
ALTER TABLE uma_conversations DROP COLUMN created_unix;
