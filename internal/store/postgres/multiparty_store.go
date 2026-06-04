package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── MultipartyContractStore ──────────────────────────────────────────────────

// MultipartyContractStore is a pool-backed store for multi-party contracts.
type MultipartyContractStore struct {
	q    querier
	pool *pgxpool.Pool
}

// NewMultipartyContractStore returns a MultipartyContractStore backed by pool.
func NewMultipartyContractStore(pool *pgxpool.Pool) *MultipartyContractStore {
	return &MultipartyContractStore{q: pool, pool: pool}
}

// Pool returns the underlying connection pool.
func (s *MultipartyContractStore) Pool() *pgxpool.Pool {
	return s.pool
}

// txMultipartyContractStore is a transaction-scoped MultipartyContractStore.
type txMultipartyContractStore struct {
	tx querier
}

func (s *txMultipartyContractStore) Create(ctx context.Context, c *domain.MultipartyContract) error {
	return createMultipartyContract(ctx, s.tx, c)
}

func (s *txMultipartyContractStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error) {
	return getMultipartyContractByID(ctx, s.tx, id)
}

func (s *txMultipartyContractStore) GetByTenderID(ctx context.Context, tenderID uuid.UUID) (*domain.MultipartyContract, error) {
	return getMultipartyContractByTenderID(ctx, s.tx, tenderID)
}

func (s *txMultipartyContractStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error) {
	return getMultipartyContractByIDForUpdate(ctx, s.tx, id)
}

func (s *txMultipartyContractStore) Update(ctx context.Context, c *domain.MultipartyContract) error {
	return updateMultipartyContract(ctx, s.tx, c)
}

// Create inserts a new multi-party contract.
func (s *MultipartyContractStore) Create(ctx context.Context, c *domain.MultipartyContract) error {
	return createMultipartyContract(ctx, s.q, c)
}

// GetByID fetches a multi-party contract by primary key.
func (s *MultipartyContractStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error) {
	return getMultipartyContractByID(ctx, s.q, id)
}

// GetByTenderID fetches the live contract for a tender.
func (s *MultipartyContractStore) GetByTenderID(ctx context.Context, tenderID uuid.UUID) (*domain.MultipartyContract, error) {
	return getMultipartyContractByTenderID(ctx, s.q, tenderID)
}

// GetByIDForUpdate fetches with SELECT ... FOR UPDATE.
func (s *MultipartyContractStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error) {
	return getMultipartyContractByIDForUpdate(ctx, s.q, id)
}

// Update persists contract status / content_hash / version / updated_at.
func (s *MultipartyContractStore) Update(ctx context.Context, c *domain.MultipartyContract) error {
	return updateMultipartyContract(ctx, s.q, c)
}

// --- helpers ---

func createMultipartyContract(ctx context.Context, q querier, c *domain.MultipartyContract) error {
	const query = `
INSERT INTO multi_party_contracts
    (id, tender_id, status, content_hash, version, currency, created_at, updated_at, deleted_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`

	_, err := q.Exec(
		ctx, query,
		c.ID, c.TenderID, string(c.Status), c.ContentHash, c.Version, c.Currency,
		c.CreatedAt, c.UpdatedAt, c.DeletedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrConflict
		}

		return fmt.Errorf("insert multi_party_contract: %w", err)
	}

	return nil
}

func getMultipartyContractByID(ctx context.Context, q querier, id uuid.UUID) (*domain.MultipartyContract, error) {
	const query = `
SELECT id, tender_id, status, content_hash, version, currency, created_at, updated_at, deleted_at
FROM multi_party_contracts
WHERE id = $1 AND deleted_at IS NULL
`

	return scanMultipartyContract(q.QueryRow(ctx, query, id))
}

func getMultipartyContractByTenderID(ctx context.Context, q querier, tenderID uuid.UUID) (*domain.MultipartyContract, error) {
	const query = `
SELECT id, tender_id, status, content_hash, version, currency, created_at, updated_at, deleted_at
FROM multi_party_contracts
WHERE tender_id = $1 AND deleted_at IS NULL
LIMIT 1
`

	return scanMultipartyContract(q.QueryRow(ctx, query, tenderID))
}

func getMultipartyContractByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.MultipartyContract, error) {
	const query = `
SELECT id, tender_id, status, content_hash, version, currency, created_at, updated_at, deleted_at
FROM multi_party_contracts
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE
`

	return scanMultipartyContract(q.QueryRow(ctx, query, id))
}

func updateMultipartyContract(ctx context.Context, q querier, c *domain.MultipartyContract) error {
	const query = `
UPDATE multi_party_contracts
SET status = $2, content_hash = $3, version = $4, currency = $5, updated_at = $6
WHERE id = $1 AND deleted_at IS NULL
`

	tag, err := q.Exec(ctx, query, c.ID, string(c.Status), c.ContentHash, c.Version, c.Currency, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update multi_party_contract: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrMultipartyContractNotFound
	}

	return nil
}

func scanMultipartyContract(row rowScanner) (*domain.MultipartyContract, error) {
	var (
		c      domain.MultipartyContract
		status string
	)

	err := row.Scan(
		&c.ID, &c.TenderID, &status, &c.ContentHash, &c.Version, &c.Currency,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMultipartyContractNotFound
		}

		return nil, fmt.Errorf("scan multi_party_contract: %w", err)
	}

	c.Status = domain.MultipartyContractStatus(status)

	return &c, nil
}

// ─── MultipartyPartyStore ─────────────────────────────────────────────────────

// MultipartyPartyStore is a pool-backed store for multi-party contract parties.
type MultipartyPartyStore struct {
	q querier
}

// NewMultipartyPartyStore returns a MultipartyPartyStore backed by pool.
func NewMultipartyPartyStore(pool *pgxpool.Pool) *MultipartyPartyStore {
	return &MultipartyPartyStore{q: pool}
}

// txMultipartyPartyStore is a transaction-scoped MultipartyPartyStore.
type txMultipartyPartyStore struct {
	tx querier
}

func (s *txMultipartyPartyStore) AddParty(ctx context.Context, p *domain.MultipartyContractParty) error {
	return addMultipartyParty(ctx, s.tx, p)
}

func (s *txMultipartyPartyStore) ListActiveByContract(
	ctx context.Context,
	contractID uuid.UUID,
) ([]*domain.MultipartyContractParty, error) {
	return listActiveMultipartyParties(ctx, s.tx, contractID)
}

func (s *txMultipartyPartyStore) SumActiveBps(ctx context.Context, contractID uuid.UUID) (int, error) {
	return sumActiveMultipartyBps(ctx, s.tx, contractID)
}

func (s *txMultipartyPartyStore) CountActiveParties(ctx context.Context, contractID uuid.UUID) (int, error) {
	return countActiveMultipartyParties(ctx, s.tx, contractID)
}

// AddParty inserts a new ACTIVE party row.
func (s *MultipartyPartyStore) AddParty(ctx context.Context, p *domain.MultipartyContractParty) error {
	return addMultipartyParty(ctx, s.q, p)
}

// ListActiveByContract returns all ACTIVE parties for a contract.
func (s *MultipartyPartyStore) ListActiveByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.MultipartyContractParty, error) {
	return listActiveMultipartyParties(ctx, s.q, contractID)
}

// SumActiveBps returns the total share_bps for ACTIVE parties of a contract.
func (s *MultipartyPartyStore) SumActiveBps(ctx context.Context, contractID uuid.UUID) (int, error) {
	return sumActiveMultipartyBps(ctx, s.q, contractID)
}

// CountActiveParties returns the count of ACTIVE parties for a contract.
func (s *MultipartyPartyStore) CountActiveParties(ctx context.Context, contractID uuid.UUID) (int, error) {
	return countActiveMultipartyParties(ctx, s.q, contractID)
}

// --- helpers ---

func addMultipartyParty(ctx context.Context, q querier, p *domain.MultipartyContractParty) error {
	const query = `
INSERT INTO multi_party_contract_parties
    (id, contract_id, vendor_user_id, role_id, share_bps, status, created_at, updated_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
`

	_, err := q.Exec(
		ctx, query,
		p.ID, p.ContractID, p.VendorUserID, p.RoleID, p.ShareBps, string(p.Status),
		p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrConflict
		}

		return fmt.Errorf("insert multi_party_contract_party: %w", err)
	}

	return nil
}

func listActiveMultipartyParties(ctx context.Context, q querier, contractID uuid.UUID) ([]*domain.MultipartyContractParty, error) {
	const query = `
SELECT id, contract_id, vendor_user_id, role_id, share_bps, status, created_at, updated_at
FROM multi_party_contract_parties
WHERE contract_id = $1 AND status = 'ACTIVE'
ORDER BY created_at ASC
`

	rows, err := q.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list active multi_party_contract_parties: %w", err)
	}

	defer rows.Close()

	var parties []*domain.MultipartyContractParty

	for rows.Next() {
		p, scanErr := scanMultipartyParty(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		parties = append(parties, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multi_party_contract_parties: %w", err)
	}

	return parties, nil
}

func sumActiveMultipartyBps(ctx context.Context, q querier, contractID uuid.UUID) (int, error) {
	const query = `
SELECT COALESCE(SUM(share_bps), 0)
FROM multi_party_contract_parties
WHERE contract_id = $1 AND status = 'ACTIVE'
`

	var sum int

	if err := q.QueryRow(ctx, query, contractID).Scan(&sum); err != nil {
		return 0, fmt.Errorf("sum active share_bps: %w", err)
	}

	return sum, nil
}

func countActiveMultipartyParties(ctx context.Context, q querier, contractID uuid.UUID) (int, error) {
	const query = `
SELECT COUNT(*)
FROM multi_party_contract_parties
WHERE contract_id = $1 AND status = 'ACTIVE'
`

	var count int

	if err := q.QueryRow(ctx, query, contractID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active parties: %w", err)
	}

	return count, nil
}

func scanMultipartyParty(row rowScanner) (*domain.MultipartyContractParty, error) {
	var (
		p      domain.MultipartyContractParty
		status string
	)

	err := row.Scan(
		&p.ID, &p.ContractID, &p.VendorUserID, &p.RoleID, &p.ShareBps, &status,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan multi_party_contract_party: %w", err)
	}

	p.Status = domain.MultipartyPartyStatus(status)

	return &p, nil
}

// ─── MultipartySignatureStore ─────────────────────────────────────────────────

// MultipartySignatureStore is a pool-backed store for multi-party contract signatures.
type MultipartySignatureStore struct {
	q querier
}

// NewMultipartySignatureStore returns a MultipartySignatureStore backed by pool.
func NewMultipartySignatureStore(pool *pgxpool.Pool) *MultipartySignatureStore {
	return &MultipartySignatureStore{q: pool}
}

// txMultipartySignatureStore is a transaction-scoped MultipartySignatureStore.
type txMultipartySignatureStore struct {
	tx querier
}

func (s *txMultipartySignatureStore) Create(ctx context.Context, sig *domain.MultipartyContractSignature) error {
	return createMultipartySignature(ctx, s.tx, sig)
}

func (s *txMultipartySignatureStore) CountSignaturesForVersion(ctx context.Context, contractID uuid.UUID, version int) (int, error) {
	return countMultipartySignaturesForVersion(ctx, s.tx, contractID, version)
}

func (s *txMultipartySignatureStore) ListByContractVersion(
	ctx context.Context,
	contractID uuid.UUID,
	version int,
) ([]*domain.MultipartyContractSignature, error) {
	return listMultipartySignaturesByContractVersion(ctx, s.tx, contractID, version)
}

// Create inserts a new multi-party signature record.
func (s *MultipartySignatureStore) Create(ctx context.Context, sig *domain.MultipartyContractSignature) error {
	return createMultipartySignature(ctx, s.q, sig)
}

// CountSignaturesForVersion returns the number of distinct signatures for (contract, version).
func (s *MultipartySignatureStore) CountSignaturesForVersion(ctx context.Context, contractID uuid.UUID, version int) (int, error) {
	return countMultipartySignaturesForVersion(ctx, s.q, contractID, version)
}

// ListByContractVersion returns all signatures for (contract, version).
func (s *MultipartySignatureStore) ListByContractVersion(
	ctx context.Context,
	contractID uuid.UUID,
	version int,
) ([]*domain.MultipartyContractSignature, error) {
	return listMultipartySignaturesByContractVersion(ctx, s.q, contractID, version)
}

// --- helpers ---

func createMultipartySignature(ctx context.Context, q querier, sig *domain.MultipartyContractSignature) error {
	const query = `
INSERT INTO multi_party_contract_signatures
    (id, contract_id, signer_user_id, version, signed_content_hash, signed_at, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7)
`

	_, err := q.Exec(
		ctx, query,
		sig.ID, sig.ContractID, sig.SignerUserID, sig.Version, sig.SignedContentHash,
		sig.SignedAt, sig.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrAlreadySigned
		}

		return fmt.Errorf("insert multi_party_contract_signature: %w", err)
	}

	return nil
}

func countMultipartySignaturesForVersion(ctx context.Context, q querier, contractID uuid.UUID, version int) (int, error) {
	const query = `
SELECT COUNT(*)
FROM multi_party_contract_signatures
WHERE contract_id = $1 AND version = $2
`

	var count int

	if err := q.QueryRow(ctx, query, contractID, version).Scan(&count); err != nil {
		return 0, fmt.Errorf("count multi_party_signatures: %w", err)
	}

	return count, nil
}

func listMultipartySignaturesByContractVersion(
	ctx context.Context,
	q querier,
	contractID uuid.UUID,
	version int,
) ([]*domain.MultipartyContractSignature, error) {
	const query = `
SELECT id, contract_id, signer_user_id, version, signed_content_hash, signed_at, created_at
FROM multi_party_contract_signatures
WHERE contract_id = $1 AND version = $2
ORDER BY signed_at ASC
LIMIT $3
`

	rows, err := q.Query(ctx, query, contractID, version, listByContractLimit)
	if err != nil {
		return nil, fmt.Errorf("list multi_party_signatures: %w", err)
	}

	defer rows.Close()

	var sigs []*domain.MultipartyContractSignature

	for rows.Next() {
		sig, scanErr := scanMultipartySignature(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		sigs = append(sigs, sig)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multi_party_signatures: %w", err)
	}

	return sigs, nil
}

func scanMultipartySignature(row rowScanner) (*domain.MultipartyContractSignature, error) {
	var sig domain.MultipartyContractSignature

	err := row.Scan(
		&sig.ID, &sig.ContractID, &sig.SignerUserID, &sig.Version, &sig.SignedContentHash,
		&sig.SignedAt, &sig.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan multi_party_contract_signature: %w", err)
	}

	return &sig, nil
}
