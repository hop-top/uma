-- Privacy: redaction lookups and export artifacts.
--
--   - uma_ingress.message_id mirrors the envelope message ID from the body,
--     so Tx.Redact finds every ingress record naming a provider message on
--     its channel without scanning bodies. Rows written before this
--     migration take it from their body. The lookup index leads with the
--     message ID, so the deduplication lookups serializable claims rely on
--     keep planning through the unique key.
--   - uma_exports holds export records; written_at is the store-clock time
--     of the first write, which the exports purge judges.
--   - a staging intent is owned by an attachment or by an export: exactly
--     one of (inbox_id, attachment_id) and export_id is set.

-- +goose Up

ALTER TABLE uma_ingress ADD COLUMN message_id text COLLATE "C" NOT NULL DEFAULT '';
UPDATE uma_ingress SET message_id = COALESCE(body -> 'input' -> 'envelope' ->> 'message_id', '');
ALTER TABLE uma_ingress ALTER COLUMN message_id DROP DEFAULT;
CREATE INDEX uma_ingress_message ON uma_ingress (message_id, tenant_id, channel_id) WHERE message_id <> '';

CREATE TABLE uma_exports (
    tenant_id  text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id         uuid NOT NULL,
    body       json NOT NULL,
    revision   text COLLATE "C" NOT NULL CHECK (revision <> ''),
    written_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX uma_exports_written ON uma_exports (written_at, tenant_id);

ALTER TABLE uma_blob_staging ALTER COLUMN inbox_id DROP NOT NULL;
ALTER TABLE uma_blob_staging ALTER COLUMN attachment_id DROP NOT NULL;
ALTER TABLE uma_blob_staging ADD COLUMN export_id uuid;
ALTER TABLE uma_blob_staging ADD CONSTRAINT uma_blob_staging_owner CHECK (
    (inbox_id IS NOT NULL AND attachment_id IS NOT NULL AND export_id IS NULL)
    OR (inbox_id IS NULL AND attachment_id IS NULL AND export_id IS NOT NULL)
);

-- +goose Down

DELETE FROM uma_blob_staging WHERE export_id IS NOT NULL;
ALTER TABLE uma_blob_staging DROP CONSTRAINT uma_blob_staging_owner;
ALTER TABLE uma_blob_staging DROP COLUMN export_id;
ALTER TABLE uma_blob_staging ALTER COLUMN attachment_id SET NOT NULL;
ALTER TABLE uma_blob_staging ALTER COLUMN inbox_id SET NOT NULL;
DROP TABLE uma_exports;
DROP INDEX uma_ingress_message;
ALTER TABLE uma_ingress DROP COLUMN message_id;
