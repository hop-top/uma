-- Event positions are assigned once the appending unit has committed.
--
--   - A unit appending events no longer reserves positions on the tenant's
--     head row. Every appending unit of a tenant wrote that one row at
--     SERIALIZABLE, so concurrent appenders failed one another whatever
--     they appended. A unit now inserts its events at placeholders:
--     negative positions, unique across the database, drawn from
--     uma_event_placeholders in append order. Readers select positive
--     positions only, so a placeholder is never read as a position.
--   - After the unit commits, a READ COMMITTED step, one at a time per
--     tenant under a transaction-scoped advisory lock, gives the tenant's
--     committed placeholders the positions after the head, a unit's events
--     adjacent and in placeholder order, continues the running RecordedAt
--     maximum along them and advances the head row in the same
--     transaction. Positions thus follow the order in which events become
--     readable, and the head, the bound of every read, only ever covers
--     assigned positions: a reader cannot move past an event that commits
--     later.
--   - A unit that reads the log after appending assigns its own events'
--     positions inside the unit, under the same lock, from the head row,
--     as every appender did before this migration.
--   - uma_events_placeholders finds the tenants holding placeholders, so a
--     step left undone by a host that stopped after a commit is redone by
--     the next store opened on the database.
--   - Down assigns every remaining placeholder its position first, the way
--     the step does.

-- +goose Up

CREATE SEQUENCE uma_event_placeholders AS bigint;

ALTER TABLE uma_events
    DROP CONSTRAINT uma_events_position_check,
    ADD CONSTRAINT uma_events_position_check CHECK (position <> 0);

CREATE INDEX uma_events_placeholders ON uma_events (tenant_id) WHERE position < 0;

-- +goose Down

WITH pending AS (
    SELECT e.tenant_id, e.position AS placeholder,
        COALESCE(h.position, 0) + row_number() OVER w AS position,
        greatest(max(e.recorded_unix::numeric * 1000000000 + e.recorded_nanos) OVER w,
            COALESCE(h.recorded_max_unix::numeric * 1000000000 + h.recorded_max_nanos, -62135596800000000000)) AS recorded_max
    FROM uma_events e
    LEFT JOIN uma_event_heads h ON h.tenant_id = e.tenant_id
    WHERE e.position < 0
    WINDOW w AS (PARTITION BY e.tenant_id ORDER BY age(e.xmin) DESC, e.position DESC ROWS UNBOUNDED PRECEDING)
), placed AS (
    UPDATE uma_events e
    SET position = p.position,
        recorded_max_unix = floor(p.recorded_max / 1000000000),
        recorded_max_nanos = p.recorded_max - floor(p.recorded_max / 1000000000) * 1000000000
    FROM pending p
    WHERE e.tenant_id = p.tenant_id AND e.position = p.placeholder
    RETURNING e.tenant_id, e.position, e.recorded_max_unix, e.recorded_max_nanos
)
INSERT INTO uma_event_heads (tenant_id, position, recorded_max_unix, recorded_max_nanos)
SELECT DISTINCT ON (tenant_id) tenant_id, position, recorded_max_unix, recorded_max_nanos
FROM placed
ORDER BY tenant_id, position DESC
ON CONFLICT (tenant_id) DO UPDATE
SET position = excluded.position, recorded_max_unix = excluded.recorded_max_unix,
    recorded_max_nanos = excluded.recorded_max_nanos;

DROP INDEX uma_events_placeholders;

ALTER TABLE uma_events
    DROP CONSTRAINT uma_events_position_check,
    ADD CONSTRAINT uma_events_position_check CHECK (position > 0);

DROP SEQUENCE uma_event_placeholders;
