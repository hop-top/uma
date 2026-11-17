-- The provider message an interaction names, so Reader.UnlinkedInteractions
-- finds the interactions of an inbox on a provider message that no local
-- message held when they were recorded, without scanning bodies.
--
--   - message_channel_tenant, message_channel_id, message_namespace,
--     message_kind, message_ext_id and message_parent_id copy every field
--     of Interaction.MessageExternal, compared exactly as uma_message_refs
--     compares a message's references; all NULL when the interaction names
--     no provider message. message_channel_id is text: PutInteraction does
--     not require an internal ID there. An interaction's provider message
--     never changes.
--   - message_linked reports that the interaction names a local message
--     (Interaction.Message). PutInteraction sets it from the record and
--     LinkInteraction sets it when it links.
--   - uma_interactions_unlinked indexes only the interactions still
--     unlinked, by provider message ID: once linked, a row leaves the
--     index, and an interaction recorded with its message never enters it,
--     so a lookup's predicate lock covers just the rows a link may change.
--   - Rows written before this migration are backfilled from their bodies.

-- +goose Up

ALTER TABLE uma_interactions
    ADD COLUMN message_channel_tenant text COLLATE "C",
    ADD COLUMN message_channel_id     text COLLATE "C",
    ADD COLUMN message_namespace      text COLLATE "C",
    ADD COLUMN message_kind           text COLLATE "C",
    ADD COLUMN message_ext_id         text COLLATE "C",
    ADD COLUMN message_parent_id      text COLLATE "C",
    ADD COLUMN message_linked         boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT uma_interactions_message_whole CHECK (
        (message_ext_id IS NULL) = (message_channel_tenant IS NULL)
        AND (message_ext_id IS NULL) = (message_channel_id IS NULL)
        AND (message_ext_id IS NULL) = (message_namespace IS NULL)
        AND (message_ext_id IS NULL) = (message_kind IS NULL)
        AND (message_ext_id IS NULL) = (message_parent_id IS NULL)
    );

UPDATE uma_interactions SET
    message_channel_tenant = coalesce(body #>> '{message_external,channel,tenant}', ''),
    message_channel_id     = coalesce(body #>> '{message_external,channel,id}', ''),
    message_namespace      = coalesce(body #>> '{message_external,namespace}', ''),
    message_kind           = coalesce(body #>> '{message_external,kind}', ''),
    message_ext_id         = coalesce(body #>> '{message_external,id}', ''),
    message_parent_id      = coalesce(body #>> '{message_external,parent_id}', '')
WHERE json_typeof(body -> 'message_external') = 'object';

UPDATE uma_interactions SET message_linked = true
WHERE json_typeof(body -> 'message') = 'object';

CREATE INDEX uma_interactions_unlinked ON uma_interactions (tenant_id, inbox_id, message_ext_id, pos)
    WHERE message_ext_id IS NOT NULL AND NOT message_linked;

-- +goose Down

DROP INDEX uma_interactions_unlinked;
ALTER TABLE uma_interactions
    DROP CONSTRAINT uma_interactions_message_whole,
    DROP COLUMN message_linked,
    DROP COLUMN message_parent_id,
    DROP COLUMN message_ext_id,
    DROP COLUMN message_kind,
    DROP COLUMN message_namespace,
    DROP COLUMN message_channel_id,
    DROP COLUMN message_channel_tenant;
