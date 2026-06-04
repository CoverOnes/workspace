package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/workspace/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MultipartyTxManager implements store.MultipartyTxManager using pgxpool.Pool.
type MultipartyTxManager struct {
	pool *pgxpool.Pool
}

// NewMultipartyTxManager returns a MultipartyTxManager backed by the given pool.
func NewMultipartyTxManager(pool *pgxpool.Pool) *MultipartyTxManager {
	return &MultipartyTxManager{pool: pool}
}

// WithMultipartyTx runs fn inside a single Postgres transaction.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
// The transaction-scoped stores passed to fn satisfy the same interfaces as the
// pool-backed stores, so service logic does not need to know whether it is inside
// a transaction.
func (m *MultipartyTxManager) WithMultipartyTx(
	ctx context.Context,
	fn func(
		ctx context.Context,
		contracts store.MultipartyContractStore,
		parties store.MultipartyPartyStore,
		sigs store.MultipartySignatureStore,
	) error,
) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin multiparty transaction: %w", err)
	}

	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			_ = rbErr
		}
	}()

	txContracts := &txMultipartyContractStore{tx: tx}
	txParties := &txMultipartyPartyStore{tx: tx}
	txSigs := &txMultipartySignatureStore{tx: tx}

	if fnErr := fn(ctx, txContracts, txParties, txSigs); fnErr != nil {
		return fnErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit multiparty transaction: %w", commitErr)
	}

	return nil
}
