-- Message provider references and the inbox disabled flag.
--
--   - uma_message_refs holds one row per entry of a message's External,
--     rewritten with the message, so Reader.MessagesByReference finds the
--     messages of an inbox holding a reference without scanning bodies.
--     Every ExternalRef field is explicit and compared exactly; the lookup
--     index leads with the provider ID.
--   - uma_inboxes.disabled mirrors Inbox.Disabled from the body.

-- +goose Up

ALTER TABLE uma_inboxes ADD COLUMN disabled boolean NOT NULL DEFAULT false;

CREATE TABLE uma_message_refs (
    tenant_id      text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id       uuid NOT NULL,
    message_id     uuid NOT NULL,
    channel_tenant text COLLATE "C" NOT NULL,
    channel_id     uuid NOT NULL,
    namespace      text COLLATE "C" NOT NULL,
    kind           text COLLATE "C" NOT NULL,
    ext_id         text COLLATE "C" NOT NULL,
    parent_id      text COLLATE "C" NOT NULL,
    FOREIGN KEY (tenant_id, message_id) REFERENCES uma_messages (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX uma_message_refs_lookup ON uma_message_refs (tenant_id, inbox_id, ext_id);
CREATE INDEX uma_message_refs_message ON uma_message_refs (tenant_id, message_id);

-- +goose Down

DROP TABLE uma_message_refs;
ALTER TABLE uma_inboxes DROP COLUMN disabled;
