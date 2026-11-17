-- Recipient logs: adapter-owned, ordered logs of what an adapter delivered
-- to one recipient it hosts itself, such as a web widget visitor.
--
--   - uma_recipient_log_heads holds one row per log with its last
--     sequence. AppendRecipientLog increments it in the appending unit, so
--     sequences are gapless per log and concurrent appends to one log
--     serialize on the row.
--   - uma_recipient_log_entries holds the retained entries; the append
--     deletes the ones older than its retain count. Entries go with their
--     head (ON DELETE CASCADE).
--   - Nothing is backfilled: logs start empty.

-- +goose Up

CREATE TABLE uma_recipient_log_heads (
    tenant_id  text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    channel_id uuid NOT NULL,
    namespace  text COLLATE "C" NOT NULL CHECK (octet_length(namespace) BETWEEN 1 AND 255),
    recipient  text COLLATE "C" NOT NULL CHECK (octet_length(recipient) BETWEEN 1 AND 1024),
    last_seq   bigint NOT NULL CHECK (last_seq > 0),
    PRIMARY KEY (tenant_id, channel_id, namespace, recipient),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES uma_channels (tenant_id, id)
);

CREATE TABLE uma_recipient_log_entries (
    tenant_id     text COLLATE "C" NOT NULL,
    channel_id    uuid NOT NULL,
    namespace     text COLLATE "C" NOT NULL,
    recipient     text COLLATE "C" NOT NULL,
    seq           bigint NOT NULL CHECK (seq > 0),
    value_name    text COLLATE "C" NOT NULL CHECK (value_name <> ''),
    value_version bigint NOT NULL CHECK (value_version BETWEEN 0 AND 4294967295),
    value_data    json,
    PRIMARY KEY (tenant_id, channel_id, namespace, recipient, seq),
    FOREIGN KEY (tenant_id, channel_id, namespace, recipient)
        REFERENCES uma_recipient_log_heads (tenant_id, channel_id, namespace, recipient) ON DELETE CASCADE
);

-- +goose Down

DROP TABLE uma_recipient_log_entries;
DROP TABLE uma_recipient_log_heads;
