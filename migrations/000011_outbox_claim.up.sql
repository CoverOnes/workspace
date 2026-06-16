-- Migration 000011: add claimed_until column to event_outbox.
-- claimed_until is set atomically by FetchPending (via UPDATE...RETURNING CTE) to
-- a short future timestamp, preventing concurrent pollers from picking up the same
-- row.  MarkPublished and RecordFailure both clear the column.
-- No FK constraints (CLAUDE.md §9).

ALTER TABLE event_outbox
    ADD COLUMN claimed_until timestamptz;

-- Replace the pending index. The original predicate (WHERE published_at IS NULL)
-- remains valid; claimed_until filtering is done in the query WHERE clause
-- (partial indexes cannot use volatile expressions like now()).
DROP INDEX IF EXISTS event_outbox_pending_idx;
CREATE INDEX event_outbox_pending_idx
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;
