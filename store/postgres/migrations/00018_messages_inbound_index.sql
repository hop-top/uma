-- Index of the live inbound messages of a conversation, by recording order.
--
--   - Reader.InboundMessages pages a conversation's inbound messages newest
--     first: no origin (not authored by uma), no echo_of, neither redacted
--     nor deleted. Without this index it reads every message of the
--     conversation to find the few that qualify, and a reply to a long
--     conversation by a bot that mostly writes would read all of it.
--   - The predicate is the query's, written the same way, so the planner
--     can use the index. ->> reads an absent key and a JSON null alike as
--     NULL, as the Go decoder reads both as nil.
--   - Nothing is backfilled in the data; the index is built over the rows
--     already there. Writes of a message that does not qualify do not touch
--     it.

-- +goose Up

CREATE INDEX uma_messages_inbound ON uma_messages (tenant_id, inbox_id, conversation_id, pos)
    WHERE body ->> 'origin' IS NULL AND body ->> 'echo_of' IS NULL
      AND body ->> 'redacted' IS DISTINCT FROM 'true' AND body ->> 'deleted' IS DISTINCT FROM 'true';

-- +goose Down

DROP INDEX uma_messages_inbound;
