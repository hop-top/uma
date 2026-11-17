-- Revisioned domain records. Get reads one record by its full ownership
-- chain; Insert creates; Update replaces under a compare-and-set on the
-- expected revision; List pages in pos order after a position.

-- name: GetChannel :one
SELECT key, type_key, config_name, config_version, config_data, credential_ref, revision
FROM uma_channels
WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertChannel :exec
INSERT INTO uma_channels (tenant_id, id, key, type_key, config_name, config_version, config_data, credential_ref, revision)
VALUES (@tenant_id, @id, @key, @type_key, @config_name, @config_version, @config_data, @credential_ref, @revision);

-- name: UpdateChannel :execrows
UPDATE uma_channels
SET key = @key, type_key = @type_key, config_name = @config_name, config_version = @config_version,
    config_data = @config_data, credential_ref = @credential_ref, revision = @revision
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: ListChannels :many
SELECT pos, id, key, type_key, config_name, config_version, config_data, credential_ref, revision
FROM uma_channels
WHERE tenant_id = @tenant_id AND pos > @after
ORDER BY pos
LIMIT @lim;

-- name: ChannelExists :one
SELECT EXISTS (SELECT 1 FROM uma_channels WHERE tenant_id = @tenant_id AND id = @id);

-- name: GetInbox :one
SELECT body, revision FROM uma_inboxes WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertInbox :exec
INSERT INTO uma_inboxes (tenant_id, id, channel_id, key, destinations, disabled, body, revision)
VALUES (@tenant_id, @id, @channel_id, @key, @destinations, @disabled, @body, @revision);

-- name: UpdateInbox :execrows
UPDATE uma_inboxes
SET channel_id = @channel_id, key = @key, destinations = @destinations, disabled = @disabled, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: ListInboxes :many
SELECT pos, body
FROM uma_inboxes
WHERE tenant_id = @tenant_id AND pos > @after
  AND (cardinality(@inboxes::text[]) = 0 OR id = ANY ((@inboxes::text[])::uuid[]))
ORDER BY pos
LIMIT @lim;

-- name: InboxExists :one
SELECT EXISTS (SELECT 1 FROM uma_inboxes WHERE tenant_id = @tenant_id AND id = @id);

-- name: ResolveDestination :many
SELECT body
FROM uma_inboxes
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND @destination::text = ANY (destinations)
ORDER BY pos
LIMIT 2;

-- name: ResolveChannelKey :one
SELECT id, key, type_key, config_name, config_version, config_data, credential_ref, revision
FROM uma_channels
WHERE tenant_id = @tenant_id AND key = @key;

-- name: ResolveInboxKey :one
SELECT id, body FROM uma_inboxes WHERE tenant_id = @tenant_id AND key = @key;

-- name: GetProtocol :one
SELECT value_name, value_version, value_data, revision
FROM uma_protocol_states
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND key = @key;

-- name: InsertProtocol :exec
INSERT INTO uma_protocol_states (tenant_id, channel_id, key, value_name, value_version, value_data, revision)
VALUES (@tenant_id, @channel_id, @key, @value_name, @value_version, @value_data, @revision);

-- name: UpdateProtocol :execrows
UPDATE uma_protocol_states
SET value_name = @value_name, value_version = @value_version, value_data = @value_data, revision = @revision
WHERE tenant_id = @tenant_id AND channel_id = @channel_id AND key = @key AND revision = @expected;

-- name: GetContact :one
SELECT body, revision FROM uma_contacts WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertContact :execrows
-- Inserts the contact unless the tenant holds its ID in the unit's
-- snapshot: no row is inserted then, as InsertMessage.
INSERT INTO uma_contacts (tenant_id, id, kind, body, revision)
VALUES (@tenant_id, @id, @kind, @body, @revision)
ON CONFLICT (tenant_id, id) DO NOTHING;

-- name: UpdateContact :execrows
UPDATE uma_contacts
SET kind = @kind, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: ListContacts :many
SELECT pos, body
FROM uma_contacts
WHERE tenant_id = @tenant_id AND pos > @after
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY (@kinds::text[]))
ORDER BY pos
LIMIT @lim;

-- name: GetConversation :one
SELECT body, revision FROM uma_conversations
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id;

-- name: InsertConversation :execrows
-- Inserts the conversation unless the tenant holds its ID in the unit's
-- snapshot: no row is inserted then, as InsertMessage.
INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, created_unix, created_nanos, body, revision,
    assignee, snooze_until_unix, snooze_until_nanos, snooze_at_unix, snooze_at_nanos, link_count)
VALUES (@tenant_id, @inbox_id, @id, @mode, @status, @created_unix, @created_nanos, @body, @revision,
    sqlc.narg(assignee)::text, sqlc.narg(snooze_until_unix)::bigint, sqlc.narg(snooze_until_nanos)::integer,
    sqlc.narg(snooze_at_unix)::bigint, sqlc.narg(snooze_at_nanos)::integer, @link_count::integer)
ON CONFLICT DO NOTHING;

-- name: UpdateConversation :execrows
UPDATE uma_conversations
SET mode = @mode, status = @status, created_unix = @created_unix, created_nanos = @created_nanos,
    body = @body, revision = @revision,
    assignee = sqlc.narg(assignee)::text,
    snooze_until_unix = sqlc.narg(snooze_until_unix)::bigint, snooze_until_nanos = sqlc.narg(snooze_until_nanos)::integer,
    snooze_at_unix = sqlc.narg(snooze_at_unix)::bigint, snooze_at_nanos = sqlc.narg(snooze_at_nanos)::integer,
    link_count = @link_count::integer
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id AND revision = @expected;

-- name: ListConversations :many
-- The label selector: a conversation holds at least one any_labels label,
-- every all_labels label (all_labels has no duplicates) and no
-- none_labels label; an empty array is no constraint.
SELECT c.pos, c.body
FROM uma_conversations c
WHERE c.tenant_id = @tenant_id AND c.pos > @after
  AND (cardinality(@inboxes::text[]) = 0 OR c.inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (cardinality(@statuses::text[]) = 0 OR c.status = ANY (@statuses::text[]))
  AND (cardinality(@any_labels::uuid[]) = 0 OR EXISTS (
    SELECT 1 FROM uma_conversation_labels l
    WHERE l.tenant_id = c.tenant_id AND l.conversation_id = c.id AND l.label_id = ANY (@any_labels::uuid[])
  ))
  AND (cardinality(@all_labels::uuid[]) = 0 OR cardinality(@all_labels::uuid[]) = (
    SELECT count(*) FROM uma_conversation_labels l
    WHERE l.tenant_id = c.tenant_id AND l.conversation_id = c.id AND l.label_id = ANY (@all_labels::uuid[])
  ))
  AND NOT EXISTS (
    SELECT 1 FROM uma_conversation_labels l
    WHERE l.tenant_id = c.tenant_id AND l.conversation_id = c.id AND l.label_id = ANY (@none_labels::uuid[])
  )
  -- The assignee filter: any assignee when assignee_any, else exactly
  -- assignee (NULL selects the unassigned).
  AND (@assignee_any::boolean OR c.assignee IS NOT DISTINCT FROM sqlc.narg(assignee)::text)
  -- The snooze filter: a snooze is in effect while it ends after now and no
  -- live inbound message (the predicate of uma_messages_inbound) that was
  -- not imported from history (empty imported_by) was recorded after it
  -- was set.
  AND (sqlc.narg(snoozed)::boolean IS NULL OR sqlc.narg(snoozed)::boolean = (
    c.snooze_until_unix IS NOT NULL
    AND (c.snooze_until_unix, c.snooze_until_nanos) > (@now_unix::bigint, @now_nanos::integer)
    AND NOT EXISTS (
      SELECT 1 FROM uma_messages m
      WHERE m.tenant_id = c.tenant_id AND m.inbox_id = c.inbox_id AND m.conversation_id = c.id
        AND m.body ->> 'origin' IS NULL AND m.body ->> 'echo_of' IS NULL
        AND m.body ->> 'redacted' IS DISTINCT FROM 'true' AND m.body ->> 'deleted' IS DISTINCT FROM 'true'
        AND COALESCE(m.body ->> 'imported_by', '') = ''
        AND (m.recorded_unix, m.recorded_nanos) > (c.snooze_at_unix, c.snooze_at_nanos)
    )
  ))
  -- The snooze-ended filter: a snooze is stored and no longer in effect, that
  -- is it ended at or before now, or a live inbound message that was not
  -- imported was recorded after it was set.
  AND (sqlc.narg(snooze_ended)::boolean IS NULL OR sqlc.narg(snooze_ended)::boolean = (
    c.snooze_until_unix IS NOT NULL
    AND (
      (c.snooze_until_unix, c.snooze_until_nanos) <= (@now_unix::bigint, @now_nanos::integer)
      OR EXISTS (
        SELECT 1 FROM uma_messages m
        WHERE m.tenant_id = c.tenant_id AND m.inbox_id = c.inbox_id AND m.conversation_id = c.id
          AND m.body ->> 'origin' IS NULL AND m.body ->> 'echo_of' IS NULL
          AND m.body ->> 'redacted' IS DISTINCT FROM 'true' AND m.body ->> 'deleted' IS DISTINCT FROM 'true'
          AND COALESCE(m.body ->> 'imported_by', '') = ''
          AND (m.recorded_unix, m.recorded_nanos) > (c.snooze_at_unix, c.snooze_at_nanos)
      )
    )
  ))
  -- The link filter: holding a link or none.
  AND (sqlc.narg(linked)::boolean IS NULL OR sqlc.narg(linked)::boolean = (c.link_count > 0))
  -- The unread filter: a message that is no echo, not imported from history
  -- and not the actor's own uma-authored message, recorded after the
  -- actor's read marker (or with no marker at all).
  AND (sqlc.narg(unread_actor)::text IS NULL OR EXISTS (
    SELECT 1 FROM uma_messages m
    WHERE m.tenant_id = c.tenant_id AND m.inbox_id = c.inbox_id AND m.conversation_id = c.id
      AND m.body ->> 'echo_of' IS NULL
      AND COALESCE(m.body ->> 'imported_by', '') = ''
      AND (m.body #>> '{origin,actor}') IS DISTINCT FROM sqlc.narg(unread_actor)::text
      AND NOT EXISTS (
        SELECT 1 FROM uma_read_markers r
        WHERE r.tenant_id = c.tenant_id AND r.conversation_id = c.id AND r.actor = sqlc.narg(unread_actor)::text
          AND (r.read_through_unix, r.read_through_nanos) >= (m.recorded_unix, m.recorded_nanos)
      )
  ))
ORDER BY c.pos
LIMIT @lim;

-- name: ConversationExists :one
SELECT EXISTS (
    SELECT 1 FROM uma_conversations WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id
);

-- name: GetMessage :one
SELECT body, revision FROM uma_messages
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id;

-- name: InsertMessage :execrows
-- Inserts the message unless the tenant holds its ID in the unit's
-- snapshot: no row is inserted then. No lookup precedes it, so it leaves
-- no predicate lock for concurrent writers' inserts to conflict with; an
-- ID a concurrent unit committed fails this unit as a serialization
-- failure.
INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, task_id, in_reply_to_id, body, revision,
    search_tokens, occurred_unix, occurred_nanos, recorded_unix, recorded_nanos)
VALUES (@tenant_id, @inbox_id, @conversation_id, @id, sqlc.narg(task_id)::uuid, sqlc.narg(in_reply_to_id)::uuid, @body, @revision,
    @search_tokens::text[], @occurred_unix, @occurred_nanos, @recorded_unix::bigint, @recorded_nanos::integer)
ON CONFLICT (tenant_id, id) DO NOTHING;

-- name: UpdateMessage :execrows
UPDATE uma_messages
SET task_id = sqlc.narg(task_id)::uuid, in_reply_to_id = sqlc.narg(in_reply_to_id)::uuid, body = @body, revision = @revision,
    search_tokens = @search_tokens::text[], occurred_unix = @occurred_unix, occurred_nanos = @occurred_nanos,
    recorded_unix = @recorded_unix::bigint, recorded_nanos = @recorded_nanos::integer
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: ListMessages :many
-- The search predicates follow store/internal/search: tokens by
-- containment, OccurredAt half-open on exact (seconds, nanos), Has on the
-- holder index, media types normalized as search.MediaType (redacted
-- record: none) and matched as search.MatchMediaType. The label selector
-- (as ListConversations) is evaluated on the labels the message's
-- conversation holds now, by join: no label is copied onto a message.
SELECT m.pos, m.body
FROM uma_messages m
WHERE m.tenant_id = @tenant_id AND m.pos > @after
  AND (cardinality(@inboxes::text[]) = 0 OR m.inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (sqlc.narg(conversation_inbox)::uuid IS NULL
       OR (m.inbox_id = sqlc.narg(conversation_inbox)::uuid AND m.conversation_id = sqlc.narg(conversation_id)::uuid))
  AND (sqlc.narg(task_id)::uuid IS NULL
       OR (m.task_id = sqlc.narg(task_id)::uuid
           AND m.inbox_id = sqlc.narg(task_inbox)::uuid AND m.conversation_id = sqlc.narg(task_conversation)::uuid))
  AND (cardinality(@any_labels::uuid[]) = 0 OR EXISTS (
    SELECT 1 FROM uma_conversation_labels l
    WHERE l.tenant_id = m.tenant_id AND l.conversation_id = m.conversation_id AND l.label_id = ANY (@any_labels::uuid[])
  ))
  AND (cardinality(@all_labels::uuid[]) = 0 OR cardinality(@all_labels::uuid[]) = (
    SELECT count(*) FROM uma_conversation_labels l
    WHERE l.tenant_id = m.tenant_id AND l.conversation_id = m.conversation_id AND l.label_id = ANY (@all_labels::uuid[])
  ))
  AND NOT EXISTS (
    SELECT 1 FROM uma_conversation_labels l
    WHERE l.tenant_id = m.tenant_id AND l.conversation_id = m.conversation_id AND l.label_id = ANY (@none_labels::uuid[])
  )
  AND (cardinality(@tokens::text[]) = 0 OR m.search_tokens @> @tokens::text[])
  AND (sqlc.narg(since_unix)::bigint IS NULL
       OR (m.occurred_unix, m.occurred_nanos) >= (sqlc.narg(since_unix)::bigint, sqlc.narg(since_nanos)::integer))
  AND (sqlc.narg(before_unix)::bigint IS NULL
       OR (m.occurred_unix, m.occurred_nanos) < (sqlc.narg(before_unix)::bigint, sqlc.narg(before_nanos)::integer))
  AND (sqlc.narg(has_attachments)::boolean IS NULL
       OR sqlc.narg(has_attachments)::boolean = EXISTS (
           SELECT 1 FROM uma_message_attachments a WHERE a.tenant_id = m.tenant_id AND a.message_id = m.id))
  AND (cardinality(@media_exact::text[]) + cardinality(@media_types::text[]) = 0
       OR EXISTS (
           SELECT 1
           FROM uma_message_attachments a
           JOIN uma_attachments r
             ON r.tenant_id = a.tenant_id AND r.inbox_id = a.inbox_id::uuid AND r.id = a.attachment_id::uuid
           CROSS JOIN LATERAL (
               SELECT translate(btrim(split_part(r.body -> 'attachment' -> 'metadata' ->> 'media_type', ';', 1), E' \t'),
                   'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') AS media_type
           ) n
           WHERE a.tenant_id = m.tenant_id AND a.message_id = m.id
             AND r.body ->> 'state' IS DISTINCT FROM 'redacted'
             AND n.media_type <> ''
             AND (n.media_type = ANY (@media_exact::text[])
                  OR EXISTS (SELECT 1 FROM unnest(@media_types::text[]) AS p(typ) WHERE starts_with(n.media_type, p.typ || '/')))))
ORDER BY m.pos
LIMIT @lim;

-- name: ListReplies :many
SELECT pos, body
FROM uma_messages
WHERE tenant_id = @tenant_id AND in_reply_to_id = @parent_id AND pos > @after
  AND inbox_id = @parent_inbox::uuid AND conversation_id = @parent_conversation::uuid
ORDER BY pos
LIMIT @lim;

-- name: GetWorkItem :one
SELECT body, revision FROM uma_work_items
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id;

-- name: InsertWorkItem :exec
INSERT INTO uma_work_items (tenant_id, inbox_id, conversation_id, id, status, body, revision)
VALUES (@tenant_id, @inbox_id, @conversation_id, @id, @status, @body, @revision);

-- name: UpdateWorkItem :execrows
UPDATE uma_work_items
SET status = @status, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: ListWorkItems :many
SELECT pos, body
FROM uma_work_items
WHERE tenant_id = @tenant_id AND pos > @after
  AND (cardinality(@inboxes::text[]) = 0 OR inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (sqlc.narg(conversation_inbox)::uuid IS NULL
       OR (inbox_id = sqlc.narg(conversation_inbox)::uuid AND conversation_id = sqlc.narg(conversation_id)::uuid))
  AND (cardinality(@statuses::text[]) = 0 OR status = ANY (@statuses::text[]))
ORDER BY pos
LIMIT @lim;

-- name: GetTask :one
SELECT body, revision FROM uma_tasks
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id;

-- name: GetTaskByReference :one
SELECT id, body FROM uma_tasks
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id
  AND external_channel_id = @external_channel_id AND external_namespace = @external_namespace
  AND external_kind = @external_kind AND external_id = @external_id AND external_parent_id = @external_parent_id;

-- name: InsertTask :exec
INSERT INTO uma_tasks (tenant_id, inbox_id, conversation_id, id, state,
    external_channel_id, external_namespace, external_kind, external_id, external_parent_id,
    status_unix, status_nanos, owner_inbox_id, owner_id, body, revision)
VALUES (@tenant_id, @inbox_id, @conversation_id, @id, @state,
    sqlc.narg(external_channel_id)::uuid, sqlc.narg(external_namespace)::text, sqlc.narg(external_kind)::text,
    sqlc.narg(external_id)::text, sqlc.narg(external_parent_id)::text,
    @status_unix, @status_nanos, sqlc.narg(owner_inbox_id)::uuid, sqlc.narg(owner_id)::uuid, @body, @revision);

-- name: UpdateTask :execrows
UPDATE uma_tasks
SET state = @state,
    external_channel_id = sqlc.narg(external_channel_id)::uuid, external_namespace = sqlc.narg(external_namespace)::text,
    external_kind = sqlc.narg(external_kind)::text, external_id = sqlc.narg(external_id)::text,
    external_parent_id = sqlc.narg(external_parent_id)::text,
    status_unix = @status_unix, status_nanos = @status_nanos,
    owner_inbox_id = sqlc.narg(owner_inbox_id)::uuid, owner_id = sqlc.narg(owner_id)::uuid,
    body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: SetTaskOwner :exec
UPDATE uma_tasks
SET owner_inbox_id = sqlc.narg(owner_inbox_id)::uuid, owner_id = sqlc.narg(owner_id)::uuid
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id
  AND (owner_inbox_id, owner_id) IS DISTINCT FROM (sqlc.narg(owner_inbox_id)::uuid, sqlc.narg(owner_id)::uuid);

-- name: ListTasks :many
SELECT status_unix, status_nanos, id, body
FROM uma_tasks
WHERE tenant_id = @tenant_id
  AND (NOT @paged::boolean OR (status_unix, status_nanos, id) < (@after_unix::bigint, @after_nanos::integer, @after_id::uuid))
  AND (cardinality(@inboxes::text[]) = 0 OR inbox_id = ANY ((@inboxes::text[])::uuid[]))
  AND (sqlc.narg(conversation_inbox)::uuid IS NULL
       OR (inbox_id = sqlc.narg(conversation_inbox)::uuid AND conversation_id = sqlc.narg(conversation_id)::uuid))
  AND (cardinality(@statuses::text[]) = 0 OR state = ANY (@statuses::text[]))
  AND (sqlc.narg(owner_id)::uuid IS NULL
       OR (owner_inbox_id = sqlc.narg(owner_inbox_id)::uuid AND owner_id = sqlc.narg(owner_id)::uuid))
ORDER BY status_unix DESC, status_nanos DESC, id DESC
LIMIT @lim;

-- name: GetStream :one
SELECT body, revision FROM uma_streams
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id;

-- name: StreamsOf :many
SELECT body FROM uma_streams
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id
  AND body->>'state' IN ('opening', 'open', 'ending')
ORDER BY id;

-- name: InsertStream :exec
INSERT INTO uma_streams (tenant_id, inbox_id, conversation_id, id, body, revision)
VALUES (@tenant_id, @inbox_id, @conversation_id, @id, @body, @revision);

-- name: UpdateStream :execrows
UPDATE uma_streams
SET body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND conversation_id = @conversation_id AND id = @id
  AND revision = @expected;

-- name: GetAttachment :one
SELECT body, revision FROM uma_attachments
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id;

-- name: InsertAttachment :exec
INSERT INTO uma_attachments (tenant_id, inbox_id, id, body, revision)
VALUES (@tenant_id, @inbox_id, @id, @body, @revision);

-- name: UpdateAttachment :execrows
UPDATE uma_attachments
SET body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND inbox_id = @inbox_id AND id = @id AND revision = @expected;

-- name: GetSubscription :one
SELECT body, revision FROM uma_subscriptions WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertSubscription :exec
INSERT INTO uma_subscriptions (tenant_id, id, consumer, body, revision)
VALUES (@tenant_id, @id, @consumer, @body, @revision);

-- name: UpdateSubscription :execrows
UPDATE uma_subscriptions
SET consumer = @consumer, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND id = @id AND revision = @expected;

-- name: DeleteMessageRefs :exec
DELETE FROM uma_message_refs WHERE tenant_id = @tenant_id AND message_id = @message_id;

-- name: InsertMessageRefs :exec
INSERT INTO uma_message_refs (tenant_id, inbox_id, message_id, channel_tenant, channel_id, namespace, kind, ext_id, parent_id)
SELECT @tenant_id, @inbox_id, @message_id,
    unnest(@channel_tenants::text[]), unnest(@channel_ids::uuid[]), unnest(@namespaces::text[]),
    unnest(@kinds::text[]), unnest(@ext_ids::text[]), unnest(@parent_ids::text[]);

-- name: DeleteMessageAttachments :exec
DELETE FROM uma_message_attachments WHERE tenant_id = @tenant_id AND message_id = @message_id;

-- name: InsertMessageAttachments :exec
INSERT INTO uma_message_attachments (tenant_id, message_id, inbox_id, attachment_id)
SELECT @tenant_id, @message_id, unnest(@inbox_ids::text[]), unnest(@attachment_ids::text[]);

-- name: ListMessagesByAttachment :many
SELECT m.pos, m.body
FROM uma_message_attachments a
JOIN uma_messages m ON m.tenant_id = a.tenant_id AND m.id = a.message_id
WHERE a.tenant_id = @tenant_id AND a.attachment_id = @attachment_id AND a.inbox_id = @inbox_id
  AND m.pos > @after
ORDER BY m.pos
LIMIT @lim;

-- name: MessagesByReference :many
-- The reference rows are read first and the messages they name by key: a
-- planner left to choose reads the inbox's messages and probes the
-- reference index for each, leaving a predicate lock on every page of the
-- inbox's messages that each new message of the inbox lands on. A
-- message's reference rows carry its inbox, so the join needs no inbox
-- test of its own.
WITH held AS MATERIALIZED (
    SELECT DISTINCT r.message_id
    FROM uma_message_refs r
    WHERE r.tenant_id = @tenant_id AND r.inbox_id = @inbox_id AND r.ext_id = @ext_id
      AND r.channel_tenant = @channel_tenant AND r.channel_id = @channel_id
      AND r.namespace = @namespace AND r.kind = @kind AND r.parent_id = @parent_id
)
SELECT m.body
FROM held h
JOIN uma_messages m ON m.tenant_id = @tenant_id AND m.id = h.message_id
LIMIT @lim;

-- name: ListInboundMessages :many
-- The predicate is the partial index uma_messages_inbound's, written the
-- same way (migration 00018).
SELECT m.pos, m.body
FROM uma_messages m
WHERE m.tenant_id = @tenant_id AND m.inbox_id = @inbox_id AND m.conversation_id = @conversation_id
  AND m.pos < @before
  AND m.body ->> 'origin' IS NULL AND m.body ->> 'echo_of' IS NULL
  AND m.body ->> 'redacted' IS DISTINCT FROM 'true' AND m.body ->> 'deleted' IS DISTINCT FROM 'true'
ORDER BY m.pos DESC
LIMIT @lim;

-- name: GetReadMarker :one
SELECT body, revision FROM uma_read_markers
WHERE tenant_id = @tenant_id AND conversation_id = @conversation_id AND actor = @actor;

-- name: InsertReadMarker :exec
INSERT INTO uma_read_markers (tenant_id, inbox_id, conversation_id, actor, read_through_unix, read_through_nanos, body, revision)
VALUES (@tenant_id, @inbox_id, @conversation_id, @actor, @read_through_unix, @read_through_nanos, @body, @revision);

-- name: UpdateReadMarker :execrows
UPDATE uma_read_markers
SET read_through_unix = @read_through_unix, read_through_nanos = @read_through_nanos, body = @body, revision = @revision
WHERE tenant_id = @tenant_id AND conversation_id = @conversation_id AND actor = @actor AND revision = @expected;
