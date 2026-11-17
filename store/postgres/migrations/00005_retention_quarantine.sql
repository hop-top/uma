-- Retention purge and work quarantine.
--
--   - written_at on uma_events, uma_operations, uma_attempt_evidence and
--     uma_ingress is the store-clock time of the write Purge judges. Rows
--     written before this migration take the migration time, so they stay
--     for at least one full retention horizon after it.
--   - uma_event_heads.base is the position of the last purged event: the
--     earliest cursor Events accepts.
--   - uma_work.claims counts an item's claims since it was enqueued or
--     requeued; uma_work.quarantined takes it out of the claimable queue.

-- +goose Up

ALTER TABLE uma_events ADD COLUMN written_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE uma_events ALTER COLUMN written_at DROP DEFAULT;
ALTER TABLE uma_operations ADD COLUMN written_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE uma_operations ALTER COLUMN written_at DROP DEFAULT;
ALTER TABLE uma_attempt_evidence ADD COLUMN written_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE uma_attempt_evidence ALTER COLUMN written_at DROP DEFAULT;
ALTER TABLE uma_ingress ADD COLUMN written_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE uma_ingress ALTER COLUMN written_at DROP DEFAULT;

-- Purge indexes lead with written_at, so key lookups, whose predicate locks
-- on the unique indexes serializable claims rely on, never plan through them.
CREATE INDEX uma_operations_written ON uma_operations (written_at, tenant_id);
CREATE INDEX uma_attempt_evidence_written ON uma_attempt_evidence (written_at, tenant_id);
CREATE INDEX uma_ingress_written ON uma_ingress (written_at, tenant_id);

ALTER TABLE uma_event_heads ADD COLUMN base bigint NOT NULL DEFAULT 0;
ALTER TABLE uma_event_heads ADD CONSTRAINT uma_event_heads_base CHECK (base >= 0 AND base <= position);

ALTER TABLE uma_work ADD COLUMN claims bigint NOT NULL DEFAULT 0 CHECK (claims >= 0);
ALTER TABLE uma_work ADD COLUMN quarantined boolean NOT NULL DEFAULT false;
CREATE INDEX uma_work_resource ON uma_work (resource_id, tenant_id, resource_kind);

-- +goose Down

DROP INDEX uma_work_resource;
ALTER TABLE uma_work DROP COLUMN quarantined;
ALTER TABLE uma_work DROP COLUMN claims;
ALTER TABLE uma_event_heads DROP CONSTRAINT uma_event_heads_base;
ALTER TABLE uma_event_heads DROP COLUMN base;
DROP INDEX uma_ingress_written;
DROP INDEX uma_attempt_evidence_written;
DROP INDEX uma_operations_written;
ALTER TABLE uma_ingress DROP COLUMN written_at;
ALTER TABLE uma_attempt_evidence DROP COLUMN written_at;
ALTER TABLE uma_operations DROP COLUMN written_at;
ALTER TABLE uma_events DROP COLUMN written_at;
