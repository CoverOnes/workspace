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

// MilestoneStore is a pool-backed store for multiparty contract milestones.
type MilestoneStore struct {
	q    querier
	pool *pgxpool.Pool
}

// NewMilestoneStore returns a MilestoneStore backed by pool.
func NewMilestoneStore(pool *pgxpool.Pool) *MilestoneStore {
	return &MilestoneStore{q: pool, pool: pool}
}

// Create inserts a new milestone row.
func (s *MilestoneStore) Create(ctx context.Context, m *domain.Milestone) error {
	const query = `
INSERT INTO multiparty_milestones
    (id, multi_contract_id, name, amount, currency, sequence, status, completed_at, created_at, updated_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`

	_, err := s.q.Exec(
		ctx, query,
		m.ID, m.MultiContractID, m.Name, m.Amount, m.Currency, m.Sequence,
		string(m.Status), m.CompletedAt, m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert multiparty_milestone: %w", err)
	}

	return nil
}

// GetByID fetches a milestone by primary key.
func (s *MilestoneStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Milestone, error) {
	const query = `
SELECT id, multi_contract_id, name, amount, currency, sequence, status, completed_at, created_at, updated_at
FROM multiparty_milestones
WHERE id = $1
`

	return scanMilestone(s.q.QueryRow(ctx, query, id))
}

// ListByContract returns all milestones for a contract ordered by sequence ASC, created_at ASC.
func (s *MilestoneStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Milestone, error) {
	const query = `
SELECT id, multi_contract_id, name, amount, currency, sequence, status, completed_at, created_at, updated_at
FROM multiparty_milestones
WHERE multi_contract_id = $1
ORDER BY sequence ASC, created_at ASC
`

	rows, err := s.q.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list multiparty_milestones: %w", err)
	}

	defer rows.Close()

	var milestones []*domain.Milestone

	for rows.Next() {
		m, scanErr := scanMilestone(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		milestones = append(milestones, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multiparty_milestones: %w", err)
	}

	return milestones, nil
}

// MarkCompleted sets status=COMPLETED and completed_at for the given milestone.
// Returns ErrMilestoneNotFound if the row does not exist.
// Returns ErrMilestoneAlreadyDone if already COMPLETED.
func (s *MilestoneStore) MarkCompleted(ctx context.Context, id uuid.UUID, completedAt time.Time) (*domain.Milestone, error) {
	const query = `
UPDATE multiparty_milestones
SET status = 'COMPLETED', completed_at = $2, updated_at = $3
WHERE id = $1 AND status = 'PENDING'
RETURNING id, multi_contract_id, name, amount, currency, sequence, status, completed_at, created_at, updated_at
`

	m, err := scanMilestone(s.q.QueryRow(ctx, query, id, completedAt, time.Now().UTC()))
	if err != nil {
		if errors.Is(err, domain.ErrMilestoneNotFound) {
			// Distinguish between "row not found at all" and "row found but already COMPLETED".
			exists, existErr := s.milestoneExists(ctx, id)
			if existErr != nil {
				return nil, existErr
			}

			if exists {
				return nil, domain.ErrMilestoneAlreadyDone
			}
		}

		return nil, err
	}

	return m, nil
}

// milestoneExists checks whether a milestone with the given id exists (any status).
func (s *MilestoneStore) milestoneExists(ctx context.Context, id uuid.UUID) (bool, error) {
	const query = `SELECT EXISTS(SELECT 1 FROM multiparty_milestones WHERE id = $1)`

	var exists bool

	if err := s.q.QueryRow(ctx, query, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("check milestone exists: %w", err)
	}

	return exists, nil
}

// --- helpers ---

func scanMilestone(row rowScanner) (*domain.Milestone, error) {
	var (
		m      domain.Milestone
		status string
	)

	err := row.Scan(
		&m.ID, &m.MultiContractID, &m.Name, &m.Amount, &m.Currency, &m.Sequence,
		&status, &m.CompletedAt, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMilestoneNotFound
		}

		return nil, fmt.Errorf("scan multiparty_milestone: %w", err)
	}

	m.Status = domain.MilestoneStatus(status)

	return &m, nil
}
