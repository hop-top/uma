-- Attachment staging intents.
--
--   - uma_blob_staging holds one row per blob key the engine is about to
--     stage bytes under, recorded before any byte is written. An upload's
--     commit deletes its row; a row left past the staging horizon belongs
--     to an upload that never committed, and the retention sweep deletes
--     its bytes.
--   - written_at is the store-clock time the intent was recorded; seq is
--     the recording order.
--   - abandoned is set once a sweep claimed the row: a commit can no
--     longer bind the key.

-- +goose Up

CREATE TABLE uma_blob_staging (
    tenant_id     text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    blob_id       uuid NOT NULL,
    inbox_id      uuid NOT NULL,
    attachment_id uuid NOT NULL,
    abandoned     boolean NOT NULL DEFAULT false,
    written_at    timestamptz NOT NULL,
    seq           bigint GENERATED ALWAYS AS IDENTITY,
    PRIMARY KEY (tenant_id, blob_id)
);

-- The sweep index leads with seq, as the purge indexes lead with
-- written_at, so key lookups never plan through it.
CREATE INDEX uma_blob_staging_seq ON uma_blob_staging (seq, tenant_id);

-- +goose Down

DROP TABLE uma_blob_staging;
