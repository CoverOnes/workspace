package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// MilestoneTxManager implements store.MilestoneTxManager using pgxpool.Pool.
// It provides a transaction-scoped MultipartyContractStore (for GetByIDForUpdate) and
// a transaction-scoped MilestoneStore (for MarkCompleted) so CompleteMilestone can
// hold a row lock on the contract row while writing the milestone status — preventing
// a concurrent CancelContract from racing between the status guard and the write.
type MilestoneTxManager struct {
	pool *pgxpool.Pool
}

// NewMilestoneTxManager returns a MilestoneTxManager backed by the given pool.
func NewMilestoneTxManager(pool *pgxpool.Pool) *MilestoneTxManager {
	return &MilestoneTxManager{pool: pool}
}

// WithMilestoneTx runs fn inside a single Postgres transaction providing
// transaction-scoped MultipartyContractStore, MilestoneStore, and OutboxStore.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
// The outbox OutboxStore is included so callers can enqueue events atomically
// with the milestone write.
func (m *MilestoneTxManager) WithMilestoneTx(
	ctx context.Context,
	fn func(ctx context.Context, contracts store.MultipartyContractStore, milestones store.MilestoneStore, outbox store.OutboxStore) error,
) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin milestone transaction: %w", err)
	}

	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			_ = rbErr
		}
	}()

	txContracts := &txMultipartyContractStore{tx: tx}
	txMilestones := &txMilestoneStore{q: tx}
	txOutbox := &txOutboxStore{tx: tx}

	if fnErr := fn(ctx, txContracts, txMilestones, txOutbox); fnErr != nil {
		return fnErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit milestone transaction: %w", commitErr)
	}

	return nil
}

// txMilestoneStore wraps a pgx.Tx to implement store.MilestoneStore inside a transaction.
type txMilestoneStore struct {
	q querier
}

// Create inserts a new milestone row within a transaction.
func (s *txMilestoneStore) Create(ctx context.Context, m *domain.Milestone) error {
	ms := &MilestoneStore{q: s.q}
	return ms.Create(ctx, m)
}

// GetByID fetches a milestone by primary key within a transaction.
func (s *txMilestoneStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Milestone, error) {
	ms := &MilestoneStore{q: s.q}
	return ms.GetByID(ctx, id)
}

// ListByContract returns all milestones for a contract within a transaction.
func (s *txMilestoneStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Milestone, error) {
	ms := &MilestoneStore{q: s.q}
	return ms.ListByContract(ctx, contractID)
}

// MarkCompleted sets status=COMPLETED and completed_at within a transaction.
func (s *txMilestoneStore) MarkCompleted(ctx context.Context, id uuid.UUID, completedAt time.Time) (*domain.Milestone, error) {
	ms := &MilestoneStore{q: s.q}
	return ms.MarkCompleted(ctx, id, completedAt)
}

// SumAmountsByContract returns the sum of ALL milestone amounts within a transaction.
func (s *txMilestoneStore) SumAmountsByContract(ctx context.Context, contractID uuid.UUID) (decimal.Decimal, error) {
	ms := &MilestoneStore{q: s.q}
	return ms.SumAmountsByContract(ctx, contractID)
}
