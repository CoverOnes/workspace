ALTER TABLE event_outbox DROP COLUMN IF EXISTS claimed_until;

DROP INDEX IF EXISTS event_outbox_pending_idx;
CREATE INDEX event_outbox_pending_idx
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;
