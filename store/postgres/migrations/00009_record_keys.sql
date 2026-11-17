-- Host-set channel and inbox keys.
--
--   - key is the record's optional Key; '' means none. Rows written before
--     this migration hold none.
--   - A non-empty key is unique among the tenant's channels, and among its
--     inboxes, through a partial unique index that also serves the lookups.

-- +goose Up

ALTER TABLE uma_channels ADD COLUMN key text COLLATE "C" NOT NULL DEFAULT '';
CREATE UNIQUE INDEX uma_channels_key ON uma_channels (tenant_id, key) WHERE key <> '';
ALTER TABLE uma_inboxes ADD COLUMN key text COLLATE "C" NOT NULL DEFAULT '';
CREATE UNIQUE INDEX uma_inboxes_key ON uma_inboxes (tenant_id, key) WHERE key <> '';

-- +goose Down

DROP INDEX uma_inboxes_key;
ALTER TABLE uma_inboxes DROP COLUMN key;
DROP INDEX uma_channels_key;
ALTER TABLE uma_channels DROP COLUMN key;
