-- Foreign keys of the rows written with a new message or conversation are
-- checked at commit.
--
--   - A message's reference, attachment and claim rows, and a
--     conversation's claim row, each reference the message or
--     conversation the same call inserts just before them. An immediate
--     check reads the parent's key index at once: on a small table, or
--     with time-ordered IDs, every new parent shares that index page, so
--     two units each writing new messages with references, or new
--     conversations, left one another a read of the page the other then
--     inserted into, and one failed with a serialization failure although
--     they wrote different rows. Checked at commit, the read comes after
--     every write of the unit.
--   - The store writes these rows only for a parent the same call inserted
--     or found stored, so none names a missing parent: the checks only
--     guard the schema, and the deferral changes no store result. A
--     violation would fail the unit at commit instead of at the statement.
--   - ON DELETE CASCADE still acts at once: only the check is deferred.
--   - Down makes the checks immediate again.

-- +goose Up

ALTER TABLE uma_message_refs
    ALTER CONSTRAINT uma_message_refs_tenant_id_message_id_fkey DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE uma_message_attachments
    ALTER CONSTRAINT uma_message_attachments_tenant_id_message_id_fkey DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE uma_message_claims
    ALTER CONSTRAINT uma_message_claims_tenant_id_message_id_fkey DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE uma_conversation_claims
    ALTER CONSTRAINT uma_conversation_claims_tenant_id_inbox_id_conversation_id_fkey DEFERRABLE INITIALLY DEFERRED;

-- +goose Down

ALTER TABLE uma_conversation_claims
    ALTER CONSTRAINT uma_conversation_claims_tenant_id_inbox_id_conversation_id_fkey NOT DEFERRABLE;
ALTER TABLE uma_message_claims
    ALTER CONSTRAINT uma_message_claims_tenant_id_message_id_fkey NOT DEFERRABLE;
ALTER TABLE uma_message_attachments
    ALTER CONSTRAINT uma_message_attachments_tenant_id_message_id_fkey NOT DEFERRABLE;
ALTER TABLE uma_message_refs
    ALTER CONSTRAINT uma_message_refs_tenant_id_message_id_fkey NOT DEFERRABLE;
