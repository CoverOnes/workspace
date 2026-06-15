package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxStore is a pool-backed store for the event_outbox table.
// Pool-backed methods (FetchPending, MarkPublished, RecordFailure, DeleteOldPublished,
// CountStalePending) are used by the poller and janitor. Enqueue is also implemented
// on txOutboxStore (transaction-scoped) for atomic domain writes.
type OutboxStore struct {
	q    querier
	pool *pgxpool.Pool
}

// NewOutboxStore returns an OutboxStore backed by pool.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{q: pool, pool: pool}
}

// txOutboxStore wraps a querier (pgx.Tx) to implement store.OutboxStore inside a transaction.
// Only Enqueue is meaningful inside a transaction; the other methods return errors if called
// (the poller always uses the pool-backed OutboxStore).
type txOutboxStore struct {
	tx querier
}

// NewTxOutboxStore returns a transaction-scoped OutboxStore that supports only Enqueue.
// It is exported so integration tests can exercise the rollback-enqueue atomicity path
// without going through a TxManager. Accepts pgx.Tx which satisfies the internal querier
// interface used by the store layer.
func NewTxOutboxStore(tx pgx.Tx) store.OutboxStore {
	return &txOutboxStore{tx: tx}
}

// Enqueue inserts a new outbox row. Must be called inside an active transaction.
func (s *txOutboxStore) Enqueue(ctx context.Context, in *store.OutboxEnqueueInput) error {
	return enqueueOutbox(ctx, s.tx, in)
}

// FetchPending is not supported inside a transaction — the poller always uses the pool-backed store.
func (s *txOutboxStore) FetchPending(_ context.Context, _ int) ([]*domain.OutboxEntry, error) {
	return nil, fmt.Errorf("FetchPending: not supported on transaction-scoped outbox store")
}

// MarkPublished is not supported inside a transaction.
func (s *txOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error {
	return fmt.Errorf("MarkPublished: not supported on transaction-scoped outbox store")
}

// RecordFailure is not supported inside a transaction.
func (s *txOutboxStore) RecordFailure(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return fmt.Errorf("RecordFailure: not supported on transaction-scoped outbox store")
}

// DeleteOldPublished is not supported inside a transaction.
func (s *txOutboxStore) DeleteOldPublished(_ context.Context, _ time.Time) (int64, error) {
	return 0, fmt.Errorf("DeleteOldPublished: not supported on transaction-scoped outbox store")
}

// CountStalePending is not supported inside a transaction.
func (s *txOutboxStore) CountStalePending(_ context.Context, _ time.Time) (int64, error) {
	return 0, fmt.Errorf("CountStalePending: not supported on transaction-scoped outbox store")
}

// enqueueOutbox is the shared INSERT logic used by both pool and tx-scoped stores.
func enqueueOutbox(ctx context.Context, q querier, in *store.OutboxEnqueueInput) error {
	const query = `
INSERT INTO event_outbox
    (aggregate_type, aggregate_id, event_id, channel, payload, created_at, next_attempt_at)
VALUES
    ($1, $2, $3, $4, $5, now(), now())
`

	_, err := q.Exec(
		ctx, query,
		in.AggregateType,
		in.AggregateID,
		in.EventID,
		in.Channel,
		in.Payload,
	)
	if err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}

	return nil
}

// Enqueue inserts a new outbox row (pool-backed, for use outside transactions).
func (s *OutboxStore) Enqueue(ctx context.Context, in *store.OutboxEnqueueInput) error {
	return enqueueOutbox(ctx, s.q, in)
}

// claimDuration is how long a claimed row is invisible to other pollers.
const claimDuration = 30 * time.Second

// FetchPending atomically claims up to limit unpublished rows by setting
// claimed_until = now() + 30s, preventing concurrent pollers from picking up the
// same rows.  Only rows whose claimed_until IS NULL OR < now() are eligible.
// The UPDATE...RETURNING CTE executes in a single round-trip and is safe under
// concurrent poller replicas without holding a long-lived explicit transaction.
func (s *OutboxStore) FetchPending(ctx context.Context, limit int) ([]*domain.OutboxEntry, error) {
	const query = `
WITH claimed AS (
    UPDATE event_outbox
    SET claimed_until = now() + $2
    WHERE id IN (
        SELECT id
        FROM event_outbox
        WHERE published_at IS NULL
          AND next_attempt_at <= now()
          AND (claimed_until IS NULL OR claimed_until < now())
        ORDER BY created_at
        LIMIT $1
        FOR UPDATE SKIP LOCKED
    )
    RETURNING id, aggregate_type, aggregate_id, event_id, channel, payload,
              created_at, published_at, attempts, last_error, next_attempt_at
)
SELECT * FROM claimed
ORDER BY created_at
`

	rows, err := s.q.Query(ctx, query, limit, claimDuration)
	if err != nil {
		return nil, fmt.Errorf("fetch pending outbox: %w", err)
	}

	defer rows.Close()

	var entries []*domain.OutboxEntry

	for rows.Next() {
		e := &domain.OutboxEntry{}

		if scanErr := rows.Scan(
			&e.ID, &e.AggregateType, &e.AggregateID, &e.EventID, &e.Channel, &e.Payload,
			&e.CreatedAt, &e.PublishedAt, &e.Attempts, &e.LastError, &e.NextAttemptAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan outbox row: %w", scanErr)
		}

		entries = append(entries, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox rows: %w", err)
	}

	return entries, nil
}

// MarkPublished sets published_at = now() and clears claimed_until for the given row.
func (s *OutboxStore) MarkPublished(ctx context.Context, id uuid.UUID) error {
	const query = `
UPDATE event_outbox
SET published_at  = now(),
    claimed_until = NULL
WHERE id = $1
`

	tag, err := s.q.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark outbox published: row %s not found", id)
	}

	return nil
}

// RecordFailure increments attempts, sets last_error, schedules the next retry,
// and clears claimed_until so the row is visible to the next poll cycle.
func (s *OutboxStore) RecordFailure(ctx context.Context, id uuid.UUID, lastErr string, nextAttemptAt time.Time) error {
	const query = `
UPDATE event_outbox
SET attempts        = attempts + 1,
    last_error      = $2,
    next_attempt_at = $3,
    claimed_until   = NULL
WHERE id = $1
`

	tag, err := s.q.Exec(ctx, query, id, lastErr, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("record outbox failure: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record outbox failure: row %s not found", id)
	}

	return nil
}

// DeleteOldPublished deletes rows where published_at < cutoff (retention cleanup).
// Unpublished rows are never deleted.
func (s *OutboxStore) DeleteOldPublished(ctx context.Context, cutoff time.Time) (int64, error) {
	const query = `DELETE FROM event_outbox WHERE published_at IS NOT NULL AND published_at < $1`

	tag, err := s.q.Exec(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old outbox entries: %w", err)
	}

	return tag.RowsAffected(), nil
}

// CountStalePending counts unpublished rows created before cutoff.
// A non-zero result means an event has been waiting longer than expected and
// the operator should be alerted.
func (s *OutboxStore) CountStalePending(ctx context.Context, cutoff time.Time) (int64, error) {
	const query = `
SELECT COUNT(*) FROM event_outbox
WHERE published_at IS NULL AND created_at < $1
`

	var count int64

	if err := s.q.QueryRow(ctx, query, cutoff).Scan(&count); err != nil {
		return 0, fmt.Errorf("count stale outbox entries: %w", err)
	}

	return count, nil
}
