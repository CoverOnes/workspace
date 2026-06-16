-- event_outbox: transactional outbox for durable cross-service event delivery.
--
-- Events are inserted atomically with the domain write (same transaction) so no
-- event is lost if the process crashes between DB commit and the old detached
-- goroutine publish. The in-process poller reads unpublished rows, relays them to
-- Redis pub/sub, and marks them published_at.
--
-- No FK constraints per CLAUDE.md §9 — referential integrity is enforced in code.
-- Retention: 7 days after publication (see task outbox-retention in Taskfile.yml).

CREATE TABLE event_outbox (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  text        NOT NULL CHECK (char_length(aggregate_type) BETWEEN 1 AND 100),
    aggregate_id    uuid        NOT NULL,
    event_id        uuid        NOT NULL,
    channel         text        NOT NULL CHECK (char_length(channel) BETWEEN 1 AND 200),
    payload         bytea       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    attempts        integer     NOT NULL DEFAULT 0,
    last_error      text,
    next_attempt_at timestamptz NOT NULL DEFAULT now()
);

-- Consumer dedup key: event_id is the idempotency handle for downstream consumers.
CREATE UNIQUE INDEX event_outbox_event_id_unique
    ON event_outbox (event_id);

-- Poller index: fetch unpublished rows in creation order, skipping locked ones.
-- Partial index on published_at IS NULL keeps it small — published rows are excluded.
CREATE INDEX event_outbox_pending_idx
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;
