package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorklogStore is a pool-backed worklog store.
type WorklogStore struct {
	q querier
}

// NewWorklogStore returns a WorklogStore backed by pool.
func NewWorklogStore(pool *pgxpool.Pool) *WorklogStore {
	return &WorklogStore{q: pool}
}

// Create inserts a new worklog entry.
func (s *WorklogStore) Create(ctx context.Context, w *domain.Worklog) error {
	const query = `
INSERT INTO worklogs
    (id, contract_id, user_id, description, minutes, logged_at, deleted_at, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
`

	_, err := s.q.Exec(
		ctx, query,
		w.ID, w.ContractID, w.UserID, w.Description, w.Minutes,
		w.LoggedAt, w.DeletedAt, w.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert worklog: %w", err)
	}

	return nil
}

// GetByID fetches a worklog by primary key (excludes soft-deleted rows).
func (s *WorklogStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Worklog, error) {
	const query = `
SELECT id, contract_id, user_id, description, minutes, logged_at, deleted_at, created_at
FROM worklogs
WHERE id = $1 AND deleted_at IS NULL
`

	return scanWorklog(s.q.QueryRow(ctx, query, id))
}

// ListByContract returns all worklogs for a contract (live rows, newest logged_at first).
func (s *WorklogStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Worklog, error) {
	const query = `
SELECT id, contract_id, user_id, description, minutes, logged_at, deleted_at, created_at
FROM worklogs
WHERE contract_id = $1 AND deleted_at IS NULL
ORDER BY logged_at DESC
`

	rows, err := s.q.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list worklogs: %w", err)
	}

	defer rows.Close()

	var worklogs []*domain.Worklog

	for rows.Next() {
		w, scanErr := scanWorklog(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		worklogs = append(worklogs, w)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worklogs: %w", err)
	}

	return worklogs, nil
}

// SoftDelete sets deleted_at on a worklog.
func (s *WorklogStore) SoftDelete(ctx context.Context, id uuid.UUID) error {
	const query = `
UPDATE worklogs
SET deleted_at = $2
WHERE id = $1 AND deleted_at IS NULL
`

	tag, err := s.q.Exec(ctx, query, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("soft delete worklog: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrWorklogNotFound
	}

	return nil
}

func scanWorklog(row rowScanner) (*domain.Worklog, error) {
	var w domain.Worklog

	err := row.Scan(
		&w.ID, &w.ContractID, &w.UserID, &w.Description, &w.Minutes,
		&w.LoggedAt, &w.DeletedAt, &w.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorklogNotFound
		}

		return nil, fmt.Errorf("scan worklog: %w", err)
	}

	return &w, nil
}
