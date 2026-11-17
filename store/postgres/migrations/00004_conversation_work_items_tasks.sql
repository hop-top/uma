-- Conversation-scoped work item and task lists.
--
--   - A conversation holds at most one work item: uma_work_items is unique
--     per (tenant, inbox, conversation). The constraint's index also serves
--     the bound-work-item lookup ingress runs for every work item
--     observation, instead of a scan of the tenant's rows.
--   - Task lists filtered by conversation are served in pos order from an
--     index leading with the conversation's full ownership chain, like
--     uma_messages_conversation.

-- +goose Up

ALTER TABLE uma_work_items
    ADD CONSTRAINT uma_work_items_conversation UNIQUE (tenant_id, inbox_id, conversation_id);
CREATE INDEX uma_tasks_conversation ON uma_tasks (tenant_id, inbox_id, conversation_id, pos);

-- +goose Down

DROP INDEX uma_tasks_conversation;
ALTER TABLE uma_work_items DROP CONSTRAINT uma_work_items_conversation;
