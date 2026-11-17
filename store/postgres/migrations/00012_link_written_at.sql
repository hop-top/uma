-- Orphan-contact purge.
--
--   - written_at on uma_identity_links is the store-clock time of the
--     write. Purge judges a contact by the latest link record leaving it
--     (from_contact), so the index leads with the contact. Rows written
--     before this migration take the migration time, so a contact
--     orphaned before it stays for at least one full retention horizon
--     after it.

-- +goose Up

ALTER TABLE uma_identity_links ADD COLUMN written_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE uma_identity_links ALTER COLUMN written_at DROP DEFAULT;
CREATE INDEX uma_identity_links_from_written ON uma_identity_links (tenant_id, from_contact, written_at);

-- +goose Down

DROP INDEX uma_identity_links_from_written;
ALTER TABLE uma_identity_links DROP COLUMN written_at;
