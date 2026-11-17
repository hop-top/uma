-- Message search.
--
--   - search_tokens holds a message's distinct search tokens, computed by
--     the shared Go tokenizer (model.SearchTokens) at every message write,
--     empty for a redacted or deleted message. Reader.Messages matches
--     Filter.Text by array containment through a GIN index; the tokens are
--     stored verbatim, so no text search configuration or extension is
--     involved.
--   - SQL cannot reproduce the tokenizer, so rows written before this
--     migration keep NULL tokens here and Migrate fills them from their
--     bodies right after applying the schema. Open refuses a database that
--     still holds a NULL row; the partial index keeps that check and the
--     fill cheap.
--   - occurred_unix and occurred_nanos are the message's OccurredAt as Unix
--     seconds and the nanosecond within the second, exact like the
--     conversation creation order (00007), so Filter.Since and
--     Filter.Before compare as Go's time comparison does. Rows written
--     before this migration are backfilled from the stored body's
--     occurred_at; a body without one reads as the zero time, as Go
--     decodes it.
--   - Attachment predicates need no column: they read the holder index
--     (uma_message_attachments) and the attachment record's media type.

-- +goose Up

ALTER TABLE uma_messages ADD COLUMN search_tokens text[] COLLATE "C";
ALTER TABLE uma_messages ADD COLUMN occurred_unix bigint NOT NULL DEFAULT 0;
ALTER TABLE uma_messages ADD COLUMN occurred_nanos integer NOT NULL DEFAULT 0
    CHECK (occurred_nanos BETWEEN 0 AND 999999999);
UPDATE uma_messages SET
    occurred_unix = extract(epoch FROM regexp_replace(coalesce(body->>'occurred_at', '0001-01-01T00:00:00Z'), '\.[0-9]+', '')::timestamptz)::bigint,
    occurred_nanos = coalesce(rpad(substring(body->>'occurred_at' FROM '\.([0-9]+)'), 9, '0'), '0')::integer;
ALTER TABLE uma_messages ALTER COLUMN occurred_unix DROP DEFAULT;
ALTER TABLE uma_messages ALTER COLUMN occurred_nanos DROP DEFAULT;
CREATE INDEX uma_messages_search ON uma_messages USING gin (search_tokens);
CREATE INDEX uma_messages_unindexed ON uma_messages (tenant_id, id) WHERE search_tokens IS NULL;

-- +goose Down

DROP INDEX uma_messages_unindexed;
DROP INDEX uma_messages_search;
ALTER TABLE uma_messages DROP COLUMN occurred_nanos;
ALTER TABLE uma_messages DROP COLUMN occurred_unix;
ALTER TABLE uma_messages DROP COLUMN search_tokens;
