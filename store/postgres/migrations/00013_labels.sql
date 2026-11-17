-- Conversation labels.
--
--   - uma_labels holds the tenant's label definitions. name_key is the
--     label name's model.LabelNameKey, unique per tenant; LabelByName looks
--     names up through the same unique index.
--   - uma_conversation_labels holds one row per label a conversation
--     holds, rewritten with the conversation from its body's labels, so the
--     conversation label selector is evaluated without scanning bodies. A
--     label a conversation holds cannot be deleted (ON DELETE RESTRICT);
--     rows go with their conversation (ON DELETE CASCADE).
--   - Conversations written before this migration hold no labels, so
--     nothing is backfilled.

-- +goose Up

CREATE TABLE uma_labels (
    tenant_id text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    id        uuid NOT NULL,
    pos       bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    name_key  text COLLATE "C" NOT NULL CHECK (name_key <> ''),
    body      json NOT NULL,
    revision  text COLLATE "C" NOT NULL CHECK (revision <> ''),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT uma_labels_name_key UNIQUE (tenant_id, name_key)
);
CREATE INDEX uma_labels_pos ON uma_labels (tenant_id, pos);

CREATE TABLE uma_conversation_labels (
    tenant_id       text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    conversation_id uuid NOT NULL,
    label_id        uuid NOT NULL,
    PRIMARY KEY (tenant_id, conversation_id, label_id),
    FOREIGN KEY (tenant_id, conversation_id) REFERENCES uma_conversations (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT uma_conversation_labels_label FOREIGN KEY (tenant_id, label_id)
        REFERENCES uma_labels (tenant_id, id) ON DELETE RESTRICT
);
CREATE INDEX uma_conversation_labels_holders ON uma_conversation_labels (tenant_id, label_id);

-- +goose Down

DROP TABLE uma_conversation_labels;
DROP TABLE uma_labels;
