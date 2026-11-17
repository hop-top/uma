-- Operation listing: Reader.Operations pages the operation ledger most
-- recently written first, by inbox, conversation, state and write time.
--
--   - inbox_id and conversation_id are the operation's scope, resolved from
--     its key and results exactly as store.OperationRecord.Scope does: the
--     inbox the target lies in (the target itself for an inbox) and the
--     conversation the target is, else the first typed result in that
--     inbox that names one (message, stream, task, a generic conversation
--     result). Both are NULL for an operation with none, a conversation
--     only with its inbox. The scope of an operation never changes, but
--     UpdateOperation rewrites the columns from the record it stores, so
--     they cannot drift from the body.
--   - state and written_at already exist (state is rewritten by every
--     UpdateOperation, written_at is the store-clock time of the last
--     write). Reads order by (written_at, id) descending, exact to the
--     microsecond the column holds.
--   - The indexes end in that order, so a page is a short range scan:
--     tenant-wide, per inbox, per conversation and per state. The purge
--     index uma_operations_written leads with written_at and stays.
--   - Rows written before this migration are backfilled from their key
--     columns and bodies, with the same rules as the Go resolution.

-- +goose Up

ALTER TABLE uma_operations
    ADD COLUMN inbox_id        uuid,
    ADD COLUMN conversation_id uuid,
    ADD CONSTRAINT uma_operations_scope CHECK (conversation_id IS NULL OR inbox_id IS NOT NULL);

UPDATE uma_operations SET inbox_id = CASE WHEN target_kind = 'inbox' THEN target_id ELSE target_inbox END;

UPDATE uma_operations o SET conversation_id = c.id::uuid
FROM (
    SELECT tenant_id, id AS operation_id,
        CASE WHEN target_kind = 'conversation' THEN target_id::text ELSE coalesce(
            CASE WHEN body #>> '{operation,result_message,conversation,inbox,id}' = inbox_id::text
                 THEN nullif(body #>> '{operation,result_message,conversation,id}', '') END,
            CASE WHEN body #>> '{operation,result_stream,conversation,inbox,id}' = inbox_id::text
                 THEN nullif(body #>> '{operation,result_stream,conversation,id}', '') END,
            CASE WHEN body #>> '{operation,result_task,conversation,inbox,id}' = inbox_id::text
                 THEN nullif(body #>> '{operation,result_task,conversation,id}', '') END,
            CASE WHEN body #>> '{operation,result,kind}' = 'conversation'
                  AND body #>> '{operation,result,inbox}' = inbox_id::text
                 THEN nullif(body #>> '{operation,result,id}', '') END
        ) END AS id
    FROM uma_operations
    WHERE inbox_id IS NOT NULL
) c
WHERE c.id IS NOT NULL AND o.tenant_id = c.tenant_id AND o.id = c.operation_id;

CREATE INDEX uma_operations_recent ON uma_operations (tenant_id, written_at DESC, id DESC);
CREATE INDEX uma_operations_inbox_recent ON uma_operations (tenant_id, inbox_id, written_at DESC, id DESC);
CREATE INDEX uma_operations_conversation_recent ON uma_operations
    (tenant_id, inbox_id, conversation_id, written_at DESC, id DESC);
CREATE INDEX uma_operations_state_recent ON uma_operations (tenant_id, state, written_at DESC, id DESC);

-- +goose Down

DROP INDEX uma_operations_state_recent;
DROP INDEX uma_operations_conversation_recent;
DROP INDEX uma_operations_inbox_recent;
DROP INDEX uma_operations_recent;
ALTER TABLE uma_operations
    DROP CONSTRAINT uma_operations_scope,
    DROP COLUMN conversation_id,
    DROP COLUMN inbox_id;
