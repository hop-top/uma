-- Recipient logs. The head row holds a log's last sequence; entries hold
-- the retained values.

-- name: NextRecipientLogSeq :one
INSERT INTO uma_recipient_log_heads (tenant_id, channel_id, namespace, recipient, last_seq)
VALUES (@tenant_id, @channel_id, @namespace, @recipient, 1)
ON CONFLICT (tenant_id, channel_id, namespace, recipient)
DO UPDATE SET last_seq = uma_recipient_log_heads.last_seq + 1
RETURNING last_seq;

-- name: InsertRecipientLogEntry :exec
INSERT INTO uma_recipient_log_entries (tenant_id, channel_id, namespace, recipient, seq, value_name, value_version, value_data)
VALUES (@tenant_id, @channel_id, @namespace, @recipient, @seq, @value_name, @value_version, @value_data);

-- name: TrimRecipientLog :exec
DELETE FROM uma_recipient_log_entries
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND namespace = @namespace AND recipient = @recipient
  AND seq <= @through;

-- name: ListRecipientLog :many
SELECT seq, value_name, value_version, value_data
FROM uma_recipient_log_entries
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND namespace = @namespace AND recipient = @recipient
  AND seq > @after
ORDER BY seq
LIMIT @lim;

-- name: EraseRecipientLog :execrows
DELETE FROM uma_recipient_log_entries
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND namespace = @namespace AND recipient = @recipient;
