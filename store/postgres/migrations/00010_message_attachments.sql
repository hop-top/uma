-- Messages holding an attachment.
--
--   - uma_message_attachments holds one row per distinct attachment a
--     message's content parts name, rewritten with the message and cleared
--     by Redact, so Reader.MessagesByAttachment finds an attachment's
--     holders without scanning bodies. inbox_id and attachment_id are the
--     attachment's; parts naming another tenant's attachment are not
--     indexed, since no lookup in this tenant can name them.
--   - Rows for messages written before this migration come from their body.

-- +goose Up

CREATE TABLE uma_message_attachments (
    tenant_id     text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    message_id    uuid NOT NULL,
    inbox_id      text COLLATE "C" NOT NULL,
    attachment_id text COLLATE "C" NOT NULL,
    PRIMARY KEY (tenant_id, message_id, inbox_id, attachment_id),
    FOREIGN KEY (tenant_id, message_id) REFERENCES uma_messages (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX uma_message_attachments_lookup ON uma_message_attachments (tenant_id, attachment_id, inbox_id);

INSERT INTO uma_message_attachments (tenant_id, message_id, inbox_id, attachment_id)
SELECT DISTINCT m.tenant_id, m.id, p -> 'attachment' -> 'inbox' ->> 'id', p -> 'attachment' ->> 'id'
FROM uma_messages m
CROSS JOIN LATERAL json_array_elements(
    CASE WHEN json_typeof(m.body -> 'content' -> 'parts') = 'array' THEN m.body -> 'content' -> 'parts' ELSE '[]'::json END
) AS p
WHERE json_typeof(p -> 'attachment') = 'object'
  AND p -> 'attachment' -> 'inbox' ->> 'tenant' = m.tenant_id
  AND p -> 'attachment' -> 'inbox' ->> 'id' IS NOT NULL
  AND p -> 'attachment' ->> 'id' IS NOT NULL;

-- +goose Down

DROP TABLE uma_message_attachments;
