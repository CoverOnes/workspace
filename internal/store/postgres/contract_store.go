package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// querier is satisfied by both pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ContractStore is a pool-backed contract store.
type ContractStore struct {
	q querier
}

// NewContractStore returns a ContractStore backed by pool.
func NewContractStore(pool *pgxpool.Pool) *ContractStore {
	return &ContractStore{q: pool}
}

// txContractStore is a transaction-scoped ContractStore.
type txContractStore struct {
	tx querier
}

func (s *txContractStore) Create(ctx context.Context, c *domain.Contract) error {
	return createContract(ctx, s.tx, c)
}

func (s *txContractStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Contract, error) {
	return getContractByID(ctx, s.tx, id)
}

func (s *txContractStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Contract, error) {
	return getContractByIDForUpdate(ctx, s.tx, id)
}

func (s *txContractStore) ListByParty(ctx context.Context, filter store.ContractFilter) ([]*domain.Contract, error) {
	return listContractsByParty(ctx, s.tx, filter)
}

func (s *txContractStore) Update(ctx context.Context, c *domain.Contract) error {
	return updateContract(ctx, s.tx, c)
}

// Create inserts a new contract.
func (s *ContractStore) Create(ctx context.Context, c *domain.Contract) error {
	return createContract(ctx, s.q, c)
}

// GetByID fetches a contract by primary key (excludes soft-deleted rows).
func (s *ContractStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Contract, error) {
	return getContractByID(ctx, s.q, id)
}

// GetByIDForUpdate fetches a contract with SELECT ... FOR UPDATE.
// This method satisfies the ContractStore interface; the row lock is only meaningful
// when called through a transaction-scoped store (txContractStore). When called on the
// pool-backed store directly the lock is acquired and released immediately, providing
// no TOCTOU protection — always use TxManager.WithTx for the dual-sign path.
func (s *ContractStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Contract, error) {
	return getContractByIDForUpdate(ctx, s.q, id)
}

// ListByParty returns contracts where caller is client or freelancer.
func (s *ContractStore) ListByParty(ctx context.Context, filter store.ContractFilter) ([]*domain.Contract, error) {
	return listContractsByParty(ctx, s.q, filter)
}

// Update persists changes to a contract.
func (s *ContractStore) Update(ctx context.Context, c *domain.Contract) error {
	return updateContract(ctx, s.q, c)
}

// --- helpers shared by pool and tx stores ---

func createContract(ctx context.Context, q querier, c *domain.Contract) error {
	const query = `
INSERT INTO contracts
    (id, listing_id, accepted_bid_id, client_user_id, freelancer_user_id,
     title, terms, amount, currency, content_hash, version, status,
     activated_at, completed_at, deleted_at, created_at, updated_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
`

	_, err := q.Exec(
		ctx, query,
		c.ID, c.ListingID, c.AcceptedBidID, c.ClientUserID, c.FreelancerUserID,
		c.Title, c.Terms, c.Amount.StringFixed(2), c.Currency, c.ContentHash, c.Version, string(c.Status),
		c.ActivatedAt, c.CompletedAt, c.DeletedAt, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrConflict
		}

		return fmt.Errorf("insert contract: %w", err)
	}

	return nil
}

func getContractByID(ctx context.Context, q querier, id uuid.UUID) (*domain.Contract, error) {
	const query = `
SELECT id, listing_id, accepted_bid_id, client_user_id, freelancer_user_id,
       title, terms, amount, currency, content_hash, version, status,
       activated_at, completed_at, deleted_at, created_at, updated_at
FROM contracts
WHERE id = $1 AND deleted_at IS NULL
`

	return scanContract(q.QueryRow(ctx, query, id))
}

func getContractByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.Contract, error) {
	const query = `
SELECT id, listing_id, accepted_bid_id, client_user_id, freelancer_user_id,
       title, terms, amount, currency, content_hash, version, status,
       activated_at, completed_at, deleted_at, created_at, updated_at
FROM contracts
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE
`

	return scanContract(q.QueryRow(ctx, query, id))
}

func listContractsByParty(ctx context.Context, q querier, filter store.ContractFilter) ([]*domain.Contract, error) {
	var (
		sb   strings.Builder
		args []any
		n    = 1
	)

	sb.WriteString(`
SELECT id, listing_id, accepted_bid_id, client_user_id, freelancer_user_id,
       title, terms, amount, currency, content_hash, version, status,
       activated_at, completed_at, deleted_at, created_at, updated_at
FROM contracts
WHERE deleted_at IS NULL
  AND (client_user_id = $1 OR freelancer_user_id = $1)`)
	args = append(args, filter.PartyUserID)
	n++

	if filter.Status != nil {
		fmt.Fprintf(&sb, " AND status = $%d", n)
		args = append(args, string(*filter.Status))
		n++
	}

	sb.WriteString(" ORDER BY created_at DESC")

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	fmt.Fprintf(&sb, " LIMIT $%d", n)
	args = append(args, limit)
	n++

	if filter.Offset > 0 {
		fmt.Fprintf(&sb, " OFFSET $%d", n)
		args = append(args, filter.Offset)
	}

	rows, err := q.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}

	defer rows.Close()

	var contracts []*domain.Contract

	for rows.Next() {
		c, scanErr := scanContract(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		contracts = append(contracts, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contracts: %w", err)
	}

	return contracts, nil
}

func updateContract(ctx context.Context, q querier, c *domain.Contract) error {
	const query = `
UPDATE contracts
SET title = $2, terms = $3, amount = $4, currency = $5, content_hash = $6,
    version = $7, status = $8, activated_at = $9, completed_at = $10,
    deleted_at = $11, updated_at = $12
WHERE id = $1 AND deleted_at IS NULL
`

	tag, err := q.Exec(
		ctx, query,
		c.ID, c.Title, c.Terms, c.Amount.StringFixed(2), c.Currency, c.ContentHash,
		c.Version, string(c.Status), c.ActivatedAt, c.CompletedAt,
		c.DeletedAt, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update contract: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrContractNotFound
	}

	return nil
}

// rowScanner is satisfied by pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanContract(row rowScanner) (*domain.Contract, error) {
	var (
		c      domain.Contract
		amount string
		status string
	)

	err := row.Scan(
		&c.ID, &c.ListingID, &c.AcceptedBidID, &c.ClientUserID, &c.FreelancerUserID,
		&c.Title, &c.Terms, &amount, &c.Currency, &c.ContentHash, &c.Version, &status,
		&c.ActivatedAt, &c.CompletedAt, &c.DeletedAt, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrContractNotFound
		}

		return nil, fmt.Errorf("scan contract: %w", err)
	}

	amt, err := decimal.NewFromString(amount)
	if err != nil {
		return nil, fmt.Errorf("parse contract amount: %w", err)
	}

	c.Amount = amt
	c.Status = domain.ContractStatus(status)

	return &c, nil
}
