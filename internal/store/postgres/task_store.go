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

// TaskStore is a pool-backed task store.
type TaskStore struct {
	q querier
}

// NewTaskStore returns a TaskStore backed by pool.
func NewTaskStore(pool *pgxpool.Pool) *TaskStore {
	return &TaskStore{q: pool}
}

// Create inserts a new task.
func (s *TaskStore) Create(ctx context.Context, t *domain.Task) error {
	const query = `
INSERT INTO tasks
    (id, contract_id, title, status, assignee_user_id, deleted_at, created_at, updated_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
`

	_, err := s.q.Exec(
		ctx, query,
		t.ID, t.ContractID, t.Title, string(t.Status),
		t.AssigneeUserID, t.DeletedAt, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}

	return nil
}

// GetByID fetches a task by primary key (excludes soft-deleted rows).
func (s *TaskStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Task, error) {
	const query = `
SELECT id, contract_id, title, status, assignee_user_id, deleted_at, created_at, updated_at
FROM tasks
WHERE id = $1 AND deleted_at IS NULL
`

	return scanTask(s.q.QueryRow(ctx, query, id))
}

// ListByContract returns all tasks for a contract (live rows, newest first).
func (s *TaskStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Task, error) {
	const query = `
SELECT id, contract_id, title, status, assignee_user_id, deleted_at, created_at, updated_at
FROM tasks
WHERE contract_id = $1 AND deleted_at IS NULL
ORDER BY created_at DESC
`

	rows, err := s.q.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	defer rows.Close()

	var tasks []*domain.Task

	for rows.Next() {
		t, scanErr := scanTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		tasks = append(tasks, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}

	return tasks, nil
}

// Update persists task changes.
func (s *TaskStore) Update(ctx context.Context, t *domain.Task) error {
	const query = `
UPDATE tasks
SET title = $2, status = $3, assignee_user_id = $4, updated_at = $5
WHERE id = $1 AND deleted_at IS NULL
`

	tag, err := s.q.Exec(
		ctx, query,
		t.ID, t.Title, string(t.Status), t.AssigneeUserID, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTaskNotFound
	}

	return nil
}

// SoftDelete sets deleted_at on a task.
func (s *TaskStore) SoftDelete(ctx context.Context, id uuid.UUID) error {
	const query = `
UPDATE tasks
SET deleted_at = $2, updated_at = $2
WHERE id = $1 AND deleted_at IS NULL
`

	tag, err := s.q.Exec(ctx, query, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("soft delete task: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTaskNotFound
	}

	return nil
}

func scanTask(row rowScanner) (*domain.Task, error) {
	var (
		t      domain.Task
		status string
	)

	err := row.Scan(
		&t.ID, &t.ContractID, &t.Title, &status, &t.AssigneeUserID,
		&t.DeletedAt, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTaskNotFound
		}

		return nil, fmt.Errorf("scan task: %w", err)
	}

	t.Status = domain.TaskStatus(status)

	return &t, nil
}
