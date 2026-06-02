package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SignatureStore is a pool-backed signature store.
type SignatureStore struct {
	q querier
}

// NewSignatureStore returns a SignatureStore backed by pool.
func NewSignatureStore(pool *pgxpool.Pool) *SignatureStore {
	return &SignatureStore{q: pool}
}

// txSignatureStore is a transaction-scoped SignatureStore.
type txSignatureStore struct {
	tx querier
}

func (s *txSignatureStore) Create(ctx context.Context, sig *domain.Signature) error {
	return createSignature(ctx, s.tx, sig)
}

func (s *txSignatureStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Signature, error) {
	return listSignaturesByContract(ctx, s.tx, contractID)
}

func (s *txSignatureStore) CountValidSignatures(ctx context.Context, contractID uuid.UUID, version int, contentHash string) (int, error) {
	return countValidSignatures(ctx, s.tx, contractID, version, contentHash)
}

// Create inserts a new signature record.
func (s *SignatureStore) Create(ctx context.Context, sig *domain.Signature) error {
	return createSignature(ctx, s.q, sig)
}

// ListByContract returns all signatures for a contract (newest signed_at first).
func (s *SignatureStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Signature, error) {
	return listSignaturesByContract(ctx, s.q, contractID)
}

// CountValidSignatures counts distinct signatures matching current version+hash.
func (s *SignatureStore) CountValidSignatures(ctx context.Context, contractID uuid.UUID, version int, contentHash string) (int, error) {
	return countValidSignatures(ctx, s.q, contractID, version, contentHash)
}

// --- helpers ---

func createSignature(ctx context.Context, q querier, sig *domain.Signature) error {
	const query = `
INSERT INTO contract_signatures
    (id, contract_id, signer_user_id, signer_role, contract_version,
     signed_content_hash, signer_ip, user_agent, signed_at, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`

	_, err := q.Exec(
		ctx, query,
		sig.ID, sig.ContractID, sig.SignerUserID, string(sig.SignerRole), sig.ContractVersion,
		sig.SignedContentHash, sig.SignerIP, sig.UserAgent, sig.SignedAt, sig.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrAlreadySigned
		}

		return fmt.Errorf("insert signature: %w", err)
	}

	return nil
}

func listSignaturesByContract(ctx context.Context, q querier, contractID uuid.UUID) ([]*domain.Signature, error) {
	const query = `
SELECT id, contract_id, signer_user_id, signer_role, contract_version,
       signed_content_hash, signer_ip, user_agent, signed_at, created_at
FROM contract_signatures
WHERE contract_id = $1
ORDER BY signed_at DESC
`

	rows, err := q.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list signatures: %w", err)
	}

	defer rows.Close()

	var sigs []*domain.Signature

	for rows.Next() {
		sig, scanErr := scanSignature(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		sigs = append(sigs, sig)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate signatures: %w", err)
	}

	return sigs, nil
}

func countValidSignatures(ctx context.Context, q querier, contractID uuid.UUID, version int, contentHash string) (int, error) {
	const query = `
SELECT COUNT(DISTINCT signer_role)
FROM contract_signatures
WHERE contract_id = $1
  AND contract_version = $2
  AND signed_content_hash = $3
`

	var count int

	if err := q.QueryRow(ctx, query, contractID, version, contentHash).Scan(&count); err != nil {
		return 0, fmt.Errorf("count valid signatures: %w", err)
	}

	return count, nil
}

func scanSignature(row rowScanner) (*domain.Signature, error) {
	var (
		sig  domain.Signature
		role string
	)

	err := row.Scan(
		&sig.ID, &sig.ContractID, &sig.SignerUserID, &role, &sig.ContractVersion,
		&sig.SignedContentHash, &sig.SignerIP, &sig.UserAgent, &sig.SignedAt, &sig.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSignatureNotFound
		}

		return nil, fmt.Errorf("scan signature: %w", err)
	}

	sig.SignerRole = domain.SignerRole(role)

	return &sig, nil
}
