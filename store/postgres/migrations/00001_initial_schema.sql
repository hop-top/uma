-- Initial uma storage schema.
--
-- Conventions:
--   - Tenants are host-supplied text keys: text COLLATE "C", 1-255 bytes.
--   - Internal IDs are uuid. The store never generates them.
--   - Every tenant-owned table has PRIMARY KEY (tenant_id, id). Child tables
--     reference their parent through composite keys that include tenant_id
--     (and inbox_id / conversation_id for inbox- and conversation-scoped
--     records), so a parent pointer can never cross tenants or inboxes.
--   - Explicit columns hold identity, constraint, filter and ordering fields.
--     `body` holds the complete record as JSON (json, not jsonb, so stored
--     extension payloads keep their bytes); reads decode it.
--   - `pos` is the list order: an identity column assigned at insert.
--   - Revisions are engine-allocated compare values; the store compares them
--     byte-exact and never generates them.

-- +goose Up

CREATE TABLE uma_channels (
    tenant_id      text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id             uuid NOT NULL,
    pos            bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    type_key       text COLLATE "C" NOT NULL,
    -- Typed, versioned channel-instance configuration: one table for every
    -- channel type, keyed by the registered configuration name and version.
    config_name    text COLLATE "C" NOT NULL,
    config_version bigint NOT NULL CHECK (config_version BETWEEN 0 AND 4294967295),
    config_data    json,
    credential_ref text COLLATE "C" NOT NULL,
    revision       text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX uma_channels_pos ON uma_channels (tenant_id, pos);

CREATE TABLE uma_inboxes (
    tenant_id    text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id           uuid NOT NULL,
    pos          bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    channel_id   uuid NOT NULL,
    destinations text[] COLLATE "C" NOT NULL,
    body         json NOT NULL,
    revision     text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES uma_channels (tenant_id, id)
);
CREATE INDEX uma_inboxes_pos ON uma_inboxes (tenant_id, pos);
CREATE INDEX uma_inboxes_channel ON uma_inboxes (tenant_id, channel_id);

CREATE TABLE uma_protocol_states (
    tenant_id     text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    channel_id    uuid NOT NULL,
    key           text COLLATE "C" NOT NULL CHECK (key <> ''),
    value_name    text COLLATE "C" NOT NULL,
    value_version bigint NOT NULL CHECK (value_version BETWEEN 0 AND 4294967295),
    value_data    json,
    revision      text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, channel_id, key),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES uma_channels (tenant_id, id)
);

CREATE TABLE uma_contacts (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id        uuid NOT NULL,
    pos       bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    kind      text COLLATE "C" NOT NULL,
    body      json NOT NULL,
    revision  text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX uma_contacts_pos ON uma_contacts (tenant_id, pos);

-- Channel identities, unique per (tenant, inbox, namespace, source ID).
-- contact_id is the single mutable link pointer. It has no foreign key: a
-- claim may precede its provisional contact, and linking never touches
-- contact records.
CREATE TABLE uma_identities (
    tenant_id  text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id   uuid NOT NULL,
    id         uuid NOT NULL,
    pos        bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    namespace  text COLLATE "C" NOT NULL CHECK (namespace <> ''),
    source_id  text COLLATE "C" NOT NULL CHECK (source_id <> ''),
    contact_id uuid NOT NULL,
    -- Latest link history record for this identity; NULL before the first.
    link_head  uuid,
    body       json NOT NULL,
    revision   text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, inbox_id, id),
    UNIQUE (tenant_id, inbox_id, namespace, source_id),
    FOREIGN KEY (tenant_id, inbox_id) REFERENCES uma_inboxes (tenant_id, id)
);
CREATE INDEX uma_identities_pos ON uma_identities (tenant_id, pos);
CREATE INDEX uma_identities_contact ON uma_identities (tenant_id, contact_id);

-- Append-only identity link history.
CREATE TABLE uma_identity_links (
    tenant_id          text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id                 uuid NOT NULL,
    seq                bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    inbox_id           uuid NOT NULL,
    identity_id        uuid NOT NULL,
    from_contact       uuid NOT NULL,
    to_contact         uuid NOT NULL,
    supersedes         uuid,
    evidence_verifier  text COLLATE "C" NOT NULL,
    evidence_reference text COLLATE "C" NOT NULL,
    body               json NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, inbox_id, identity_id) REFERENCES uma_identities (tenant_id, inbox_id, id),
    FOREIGN KEY (tenant_id, supersedes) REFERENCES uma_identity_links (tenant_id, id),
    CHECK (from_contact <> to_contact)
);
CREATE INDEX uma_identity_links_identity ON uma_identity_links (tenant_id, identity_id, seq);
CREATE INDEX uma_identity_links_from ON uma_identity_links (tenant_id, from_contact, seq);
CREATE INDEX uma_identity_links_to ON uma_identity_links (tenant_id, to_contact, seq);
-- Non-empty verifier evidence is single-use.
CREATE UNIQUE INDEX uma_identity_links_evidence ON uma_identity_links (tenant_id, evidence_verifier, evidence_reference)
    WHERE evidence_reference <> '';

CREATE TABLE uma_conversations (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id  uuid NOT NULL,
    id        uuid NOT NULL,
    pos       bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    mode      text COLLATE "C" NOT NULL,
    status    text COLLATE "C" NOT NULL,
    body      json NOT NULL,
    revision  text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, inbox_id, id),
    FOREIGN KEY (tenant_id, inbox_id) REFERENCES uma_inboxes (tenant_id, id)
);
CREATE INDEX uma_conversations_pos ON uma_conversations (tenant_id, pos);

-- Conversation creation claims. A (selection, create key) claims exactly one
-- conversation; the claims of one selection form its candidate group.
CREATE TABLE uma_conversation_claims (
    tenant_id       text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id        uuid NOT NULL,
    mode            text COLLATE "C" NOT NULL,
    selection_key   text COLLATE "C" NOT NULL CHECK (selection_key <> ''),
    create_key      text COLLATE "C" NOT NULL CHECK (create_key <> ''),
    conversation_id uuid NOT NULL,
    PRIMARY KEY (tenant_id, inbox_id, mode, selection_key, create_key),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id)
);

CREATE TABLE uma_messages (
    tenant_id       text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id        uuid NOT NULL,
    conversation_id uuid NOT NULL,
    id              uuid NOT NULL,
    pos             bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    body            json NOT NULL,
    revision        text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id)
);
CREATE INDEX uma_messages_pos ON uma_messages (tenant_id, pos);
CREATE INDEX uma_messages_conversation ON uma_messages (tenant_id, inbox_id, conversation_id, pos);

CREATE TABLE uma_work_items (
    tenant_id       text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id        uuid NOT NULL,
    conversation_id uuid NOT NULL,
    id              uuid NOT NULL,
    pos             bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    status          text COLLATE "C" NOT NULL,
    body            json NOT NULL,
    revision        text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id)
);
CREATE INDEX uma_work_items_pos ON uma_work_items (tenant_id, pos);

CREATE TABLE uma_tasks (
    tenant_id       text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id        uuid NOT NULL,
    conversation_id uuid NOT NULL,
    id              uuid NOT NULL,
    pos             bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    state           text COLLATE "C" NOT NULL,
    body            json NOT NULL,
    revision        text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id)
);
CREATE INDEX uma_tasks_pos ON uma_tasks (tenant_id, pos);

CREATE TABLE uma_streams (
    tenant_id       text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id        uuid NOT NULL,
    conversation_id uuid NOT NULL,
    id              uuid NOT NULL,
    body            json NOT NULL,
    revision        text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id)
);

CREATE TABLE uma_attachments (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id  uuid NOT NULL,
    id        uuid NOT NULL,
    body      json NOT NULL,
    revision  text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, inbox_id) REFERENCES uma_inboxes (tenant_id, id)
);

-- Accepted ingress, deduplicated per channel instance.
CREATE TABLE uma_ingress (
    tenant_id  text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id         uuid NOT NULL,
    channel_id uuid NOT NULL,
    dedup_key  text COLLATE "C" NOT NULL CHECK (dedup_key <> ''),
    body       json NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, channel_id, dedup_key),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES uma_channels (tenant_id, id)
);

-- Operation ledger. The operation key (caller, kind, target, operation ID)
-- is unique per tenant; target_inbox is NULL for tenant-scoped targets.
CREATE TABLE uma_operations (
    tenant_id           text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id                  uuid NOT NULL,
    caller              text COLLATE "C" NOT NULL CHECK (octet_length(caller) BETWEEN 1 AND 255),
    kind                text COLLATE "C" NOT NULL CHECK (kind <> ''),
    target_kind         text COLLATE "C" NOT NULL,
    target_inbox        uuid,
    target_id           uuid NOT NULL,
    operation_key       text COLLATE "C" NOT NULL CHECK (octet_length(operation_key) BETWEEN 1 AND 255),
    fingerprint_version bigint NOT NULL CHECK (fingerprint_version BETWEEN 1 AND 4294967295),
    fingerprint         bytea NOT NULL CHECK (octet_length(fingerprint) > 0),
    state               text COLLATE "C" NOT NULL,
    expires_at          timestamptz,
    body                json NOT NULL,
    revision            text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    UNIQUE NULLS NOT DISTINCT (tenant_id, caller, kind, target_kind, target_inbox, target_id, operation_key)
);

-- Attempt evidence, appended in order, also under stale fences.
CREATE TABLE uma_attempt_evidence (
    tenant_id    text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    operation_id uuid NOT NULL,
    seq          bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    attempt_id   uuid NOT NULL,
    body         json NOT NULL,
    PRIMARY KEY (tenant_id, operation_id, seq),
    FOREIGN KEY (tenant_id, operation_id) REFERENCES uma_operations (tenant_id, id)
);

-- Committed domain events. position is dense per tenant and allocated from
-- uma_event_heads: appenders to one tenant serialize on its head row, so
-- position order is commit order.
CREATE TABLE uma_event_heads (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    position  bigint NOT NULL CHECK (position >= 0),
    PRIMARY KEY (tenant_id)
);

CREATE TABLE uma_events (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    position  bigint NOT NULL CHECK (position > 0),
    id        uuid NOT NULL,
    kind      text COLLATE "C" NOT NULL CHECK (kind <> ''),
    inbox_id  uuid,
    body      json NOT NULL,
    PRIMARY KEY (tenant_id, position),
    UNIQUE (tenant_id, id)
);

CREATE TABLE uma_subscriptions (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id        uuid NOT NULL,
    consumer  text COLLATE "C" NOT NULL CHECK (consumer <> ''),
    body      json NOT NULL,
    revision  text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id)
);

-- Fenced, expiring resource leases. A row outlives its lease so generations
-- only grow; owner and until are NULL while the resource is unleased.
CREATE TABLE uma_leases (
    tenant_id      text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    resource_kind  text COLLATE "C" NOT NULL,
    resource_inbox uuid,
    resource_id    uuid NOT NULL,
    generation     bigint NOT NULL CHECK (generation > 0),
    owner          text COLLATE "C" CHECK (owner <> ''),
    until          timestamptz,
    CHECK ((owner IS NULL) = (until IS NULL)),
    UNIQUE NULLS NOT DISTINCT (tenant_id, resource_kind, resource_inbox, resource_id)
);

-- Pending background work. fence_* and until are set while claimed.
CREATE TABLE uma_work (
    tenant_id        text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id               uuid NOT NULL,
    seq              bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    kind             text COLLATE "C" NOT NULL CHECK (kind <> ''),
    resource_kind    text COLLATE "C" NOT NULL,
    resource_inbox   uuid,
    resource_id      uuid NOT NULL,
    fence_owner      text COLLATE "C",
    fence_generation bigint,
    until            timestamptz,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX uma_work_kind ON uma_work (tenant_id, kind, seq);

-- +goose Down

DROP TABLE uma_work;
DROP TABLE uma_leases;
DROP TABLE uma_subscriptions;
DROP TABLE uma_events;
DROP TABLE uma_event_heads;
DROP TABLE uma_attempt_evidence;
DROP TABLE uma_operations;
DROP TABLE uma_ingress;
DROP TABLE uma_attachments;
DROP TABLE uma_streams;
DROP TABLE uma_tasks;
DROP TABLE uma_work_items;
DROP TABLE uma_messages;
DROP TABLE uma_conversation_claims;
DROP TABLE uma_conversations;
DROP TABLE uma_identity_links;
DROP TABLE uma_identities;
DROP TABLE uma_contacts;
DROP TABLE uma_protocol_states;
DROP TABLE uma_inboxes;
DROP TABLE uma_channels;
