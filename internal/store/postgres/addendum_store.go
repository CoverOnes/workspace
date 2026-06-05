package postgres

import (
	"context"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── AddendumStore ────────────────────────────────────────────────────────────

// AddendumStore is a pool-backed store for contract addenda.
type AddendumStore struct {
	q querier
}

// NewAddendumStore returns an AddendumStore backed by pool.
func NewAddendumStore(pool *pgxpool.Pool) *AddendumStore {
	return &AddendumStore{q: pool}
}

// txAddendumStore is a transaction-scoped AddendumStore.
type txAddendumStore struct {
	tx querier
}

func (s *txAddendumStore) Create(ctx context.Context, a *domain.ContractAddendum) error {
	return createAddendum(ctx, s.tx, a)
}

func (s *txAddendumStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAddendum, error) {
	return listAddendaByContract(ctx, s.tx, contractID)
}

// Create inserts a new addendum row.
func (s *AddendumStore) Create(ctx context.Context, a *domain.ContractAddendum) error {
	return createAddendum(ctx, s.q, a)
}

// ListByContract returns all addenda for a contract ordered by created_at ASC.
func (s *AddendumStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAddendum, error) {
	return listAddendaByContract(ctx, s.q, contractID)
}

// --- helpers ---

func createAddendum(ctx context.Context, q querier, a *domain.ContractAddendum) error {
	const query = `
INSERT INTO contract_addenda
    (id, contract_id, from_version, to_version, new_party_id, new_vendor_user_id, triggered_by, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
`

	if _, err := q.Exec(
		ctx, query,
		a.ID, a.ContractID, a.FromVersion, a.ToVersion, a.NewPartyID, a.NewVendorUserID, a.TriggeredBy, a.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert contract_addendum: %w", err)
	}

	return nil
}

func listAddendaByContract(ctx context.Context, q querier, contractID uuid.UUID) ([]*domain.ContractAddendum, error) {
	const query = `
SELECT id, contract_id, from_version, to_version, new_party_id, new_vendor_user_id, triggered_by, created_at
FROM contract_addenda
WHERE contract_id = $1
ORDER BY created_at ASC
`

	rows, err := q.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list contract_addenda: %w", err)
	}

	defer rows.Close()

	var addenda []*domain.ContractAddendum

	for rows.Next() {
		var a domain.ContractAddendum

		if scanErr := rows.Scan(
			&a.ID, &a.ContractID, &a.FromVersion, &a.ToVersion,
			&a.NewPartyID, &a.NewVendorUserID, &a.TriggeredBy, &a.CreatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan contract_addendum: %w", scanErr)
		}

		addenda = append(addenda, &a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contract_addenda: %w", err)
	}

	return addenda, nil
}
