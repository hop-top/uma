-- Interaction records.
--
--   - uma_interactions holds one row per interaction, keyed by its
--     conversation and ID. body is the model.Interaction without its
--     response token. pos orders lists by recording, as the other record
--     tables do.
--   - response_token is the provider response credential, the token's
--     only location in the schema. No read of the interaction selects it:
--     one query loads it for the effector at attempt time, and redaction
--     and the expired-token sweep clear it to ''. token_expires_unix and
--     token_expires_nanos are its expiry as Unix seconds and the
--     nanosecond within the second, exact like uma_tasks status_unix; the
--     sweep clears tokens past it and keeps the row.
--   - ingress_id names the ingress record the interaction was created
--     from, so redaction finds the raw copy. It is not a foreign key:
--     ingress retention may delete the record first.
--   - actor_inbox_id and actor_id copy the actor's resolved identity, both
--     NULL when the record names none, so a list filtered by actor reads
--     one index.
--   - Nothing is backfilled: interactions start empty.

-- +goose Up

CREATE TABLE uma_interactions (
    tenant_id           text COLLATE "C" NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
    inbox_id            uuid NOT NULL,
    conversation_id     uuid NOT NULL,
    id                  uuid NOT NULL,
    pos                 bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    actor_inbox_id      uuid,
    actor_id            uuid,
    ingress_id          uuid NOT NULL,
    body                json NOT NULL,
    revision            text COLLATE "C" NOT NULL CHECK (revision <> ''),
    response_token      text COLLATE "C" NOT NULL DEFAULT '',
    token_expires_unix  bigint NOT NULL,
    token_expires_nanos integer NOT NULL CHECK (token_expires_nanos BETWEEN 0 AND 999999999),
    PRIMARY KEY (tenant_id, inbox_id, conversation_id, id),
    FOREIGN KEY (tenant_id, inbox_id, conversation_id) REFERENCES uma_conversations (tenant_id, inbox_id, id),
    CONSTRAINT uma_interactions_actor_whole CHECK ((actor_inbox_id IS NULL) = (actor_id IS NULL))
);
CREATE INDEX uma_interactions_pos ON uma_interactions (tenant_id, pos);
CREATE INDEX uma_interactions_conversation ON uma_interactions (tenant_id, inbox_id, conversation_id, pos);
CREATE INDEX uma_interactions_actor ON uma_interactions (tenant_id, actor_inbox_id, actor_id, pos) WHERE actor_id IS NOT NULL;
CREATE INDEX uma_interactions_token_expiry ON uma_interactions (tenant_id, token_expires_unix, token_expires_nanos, pos)
    WHERE response_token <> '';

-- +goose Down

DROP TABLE uma_interactions;
