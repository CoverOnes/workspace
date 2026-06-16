package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ContractProofStore is a pool-backed store for contract proof records.
// Proofs are legal artifacts; there is no soft-delete or TTL.
type ContractProofStore struct {
	pool *pgxpool.Pool
}

// NewContractProofStore returns a ContractProofStore backed by pool.
func NewContractProofStore(pool *pgxpool.Pool) *ContractProofStore {
	return &ContractProofStore{pool: pool}
}

// Create inserts a new contract proof row.
// Returns store.ErrProofAlreadyExists (mapped from 23505 unique violation on
// (contract_id, contract_kind)) when a proof already exists for this contract.
func (s *ContractProofStore) Create(ctx context.Context, p *domain.ContractProof) error {
	const query = `
INSERT INTO contract_proofs
    (id, contract_id, contract_kind, contract_version, file_id, object_key, sha256, audit_chain_head, generated_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`

	_, err := s.pool.Exec(ctx, query,
		p.ID, p.ContractID, string(p.ContractKind), p.ContractVersion,
		p.FileID, p.ObjectKey, p.SHA256, p.AuditChainHead, p.GeneratedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrProofAlreadyExists
		}

		return fmt.Errorf("insert contract_proof: %w", err)
	}

	return nil
}

// GetByContract fetches the proof row for (contractID, kind).
// Returns store.ErrProofNotFound when no row exists.
func (s *ContractProofStore) GetByContract(
	ctx context.Context,
	contractID uuid.UUID,
	kind domain.ContractKind,
) (*domain.ContractProof, error) {
	const query = `
SELECT id, contract_id, contract_kind, contract_version,
       file_id, object_key, sha256, audit_chain_head, generated_at
FROM contract_proofs
WHERE contract_id = $1 AND contract_kind = $2
`

	row := s.pool.QueryRow(ctx, query, contractID, string(kind))

	p, err := scanProof(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrProofNotFound
		}

		return nil, fmt.Errorf("get contract_proof: %w", err)
	}

	return p, nil
}

// Supersede updates an existing proof row in place with new content.
// Used when an addendum re-sign produces a new ACTIVE contract version that supersedes
// the previous proof. The unique (contract_id, contract_kind) row is updated with
// new file_id, object_key, sha256, audit_chain_head, contract_version, generated_at.
// Returns store.ErrProofNotFound when no existing row exists.
func (s *ContractProofStore) Supersede(ctx context.Context, p *domain.ContractProof) error {
	const query = `
UPDATE contract_proofs
SET file_id          = $1,
    object_key       = $2,
    sha256           = $3,
    audit_chain_head = $4,
    contract_version = $5,
    generated_at     = $6
WHERE contract_id = $7 AND contract_kind = $8
`

	tag, err := s.pool.Exec(ctx, query,
		p.FileID, p.ObjectKey, p.SHA256, p.AuditChainHead,
		p.ContractVersion, p.GeneratedAt,
		p.ContractID, string(p.ContractKind),
	)
	if err != nil {
		return fmt.Errorf("supersede contract_proof: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return store.ErrProofNotFound
	}

	return nil
}

// scanProof scans a single proof row from a pgx.Row or pgx.Rows scan.
func scanProof(row pgx.Row) (*domain.ContractProof, error) {
	var (
		p    domain.ContractProof
		kind string
	)

	if err := row.Scan(
		&p.ID, &p.ContractID, &kind, &p.ContractVersion,
		&p.FileID, &p.ObjectKey, &p.SHA256, &p.AuditChainHead,
		&p.GeneratedAt,
	); err != nil {
		return nil, err
	}

	p.ContractKind = domain.ContractKind(kind)
	p.GeneratedAt = p.GeneratedAt.UTC()

	return &p, nil
}

// isUniqueViolation returns true for a Postgres 23505 unique-constraint violation.
func isUniqueViolation(err error) bool {
	return isPgxCode(err, "23505")
}

// isPgxCode inspects a pgx error for a specific SQLSTATE code.
func isPgxCode(err error, code string) bool {
	var pgErr interface{ SQLState() string }

	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == code
	}

	return false
}

// Pool returns the underlying pool so callers (e.g. integration tests)
// can instantiate sibling stores from the same connection pool.
func (s *ContractProofStore) Pool() *pgxpool.Pool {
	return s.pool
}
