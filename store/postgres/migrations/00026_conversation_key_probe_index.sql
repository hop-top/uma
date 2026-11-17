-- Index that makes a conversation's key the planner's first choice for
-- the lookups that name one.
--
--   - A conversation is looked up by (tenant, ID), alone or with its
--     inbox: the existence check of every write into it, and the foreign
--     key checks of the message, work item, task, interaction, label, read
--     marker and claim rows that name it. Three indexes lead with the
--     tenant: the primary key, the key of (tenant, inbox, ID) and
--     uma_conversations_pos. Until the planner has statistics for the
--     table, or for a tenant that arrived since it last looked, it expects
--     one row from all three and breaks the tie for the index created
--     last; that was the position index, and the probe became a scan of
--     the tenant's conversations.
--   - At SERIALIZABLE the scan leaves a predicate lock on every
--     conversation page it read, a relation-level lock once there are
--     many, and each unit creating a conversation of the tenant then
--     conflicts with each unit writing into one, though they touch
--     different rows.
--   - uma_conversations_lookup repeats the primary key's columns and is
--     created after the position index, so the probe's key wins that tie.
--     With statistics the planner chooses by cost and either serves. The
--     index adds one entry per new conversation, as each index of the table
--     does; the columns it holds never change.
--   - Nothing is backfilled in the data; the index is built over the rows
--     already there, holding writers off the table while it builds.
--   - Down drops it.

-- +goose Up

CREATE INDEX uma_conversations_lookup ON uma_conversations (tenant_id, id);

-- +goose Down

DROP INDEX uma_conversations_lookup;
