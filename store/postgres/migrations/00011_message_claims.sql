-- Message identity claims.
--
--   - uma_message_claims holds one row per provider-assigned message
--     identity the engine claimed: a provider reference in one inbox,
--     every ExternalRef field explicit and compared exactly. The primary
--     key keeps one message per claim; the row lasts as long as its
--     message. Messages stored before this migration hold no claim.

-- +goose Up

CREATE TABLE uma_message_claims (
    tenant_id      text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id       uuid NOT NULL,
    channel_tenant text COLLATE "C" NOT NULL,
    channel_id     uuid NOT NULL,
    namespace      text COLLATE "C" NOT NULL,
    kind           text COLLATE "C" NOT NULL,
    ext_id         text COLLATE "C" NOT NULL,
    parent_id      text COLLATE "C" NOT NULL,
    message_id     uuid NOT NULL,
    PRIMARY KEY (tenant_id, inbox_id, channel_tenant, channel_id, namespace, kind, ext_id, parent_id),
    FOREIGN KEY (tenant_id, message_id) REFERENCES uma_messages (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX uma_message_claims_message ON uma_message_claims (tenant_id, message_id);

-- +goose Down

DROP TABLE uma_message_claims;
