// Package store defines the storage interfaces for the workspace domain.
package store

import (
	"context"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
)

// ContractStore defines persistence operations for contracts.
type ContractStore interface {
	Create(ctx context.Context, c *domain.Contract) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Contract, error)
	// GetByIDForUpdate fetches a contract by ID with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction.
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Contract, error)
	ListByParty(ctx context.Context, filter ContractFilter) ([]*domain.Contract, error)
	Update(ctx context.Context, c *domain.Contract) error
}

// ContractFilter carries optional filters for contract list queries.
type ContractFilter struct {
	PartyUserID uuid.UUID // required: caller must be a party
	Status      *domain.ContractStatus
	Limit       int
	Offset      int
}

// SignatureStore defines persistence operations for contract signatures.
type SignatureStore interface {
	Create(ctx context.Context, s *domain.Signature) error
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Signature, error)
	// CountValidSignatures returns the count of distinct valid signatures for the
	// current (contractID, version, contentHash) combination.
	CountValidSignatures(ctx context.Context, contractID uuid.UUID, version int, contentHash string) (int, error)
}

// TaskStore defines persistence operations for tasks.
type TaskStore interface {
	Create(ctx context.Context, t *domain.Task) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Task, error)
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Task, error)
	Update(ctx context.Context, t *domain.Task) error
	SoftDelete(ctx context.Context, id uuid.UUID) error
}

// WorklogStore defines persistence operations for worklogs.
type WorklogStore interface {
	Create(ctx context.Context, w *domain.Worklog) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Worklog, error)
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Worklog, error)
	SoftDelete(ctx context.Context, id uuid.UUID) error
}

// TxManager runs a function inside a single Postgres transaction providing
// transaction-scoped stores for atomic operations (e.g., dual-sign completion).
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, contracts ContractStore, signatures SignatureStore) error) error
}

// MultipartyContractStore defines persistence operations for multi-party contracts.
type MultipartyContractStore interface {
	// Create inserts a new multi-party contract. Returns ErrConflict (23505) if a
	// live contract for tender_id already exists (UNIQUE WHERE deleted_at IS NULL).
	Create(ctx context.Context, c *domain.MultipartyContract) error
	// GetByID fetches a contract by primary key (excludes soft-deleted rows).
	GetByID(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error)
	// GetByTenderID fetches the single live contract for a tender, or ErrMultipartyContractNotFound.
	GetByTenderID(ctx context.Context, tenderID uuid.UUID) (*domain.MultipartyContract, error)
	// GetByIDForUpdate fetches with SELECT ... FOR UPDATE inside an active transaction.
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error)
	// Update persists contract status, content_hash, version, and updated_at.
	Update(ctx context.Context, c *domain.MultipartyContract) error
}

// MultipartyPartyStore defines persistence operations for multi-party contract parties.
type MultipartyPartyStore interface {
	// AddParty inserts a new ACTIVE party row.
	// Returns ErrConflict (23505) if the vendor already has an ACTIVE row for this contract.
	AddParty(ctx context.Context, p *domain.MultipartyContractParty) error
	// ListActiveByContract returns all ACTIVE parties for a contract.
	ListActiveByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.MultipartyContractParty, error)
	// SumActiveBps returns the sum of share_bps for all ACTIVE parties of a contract.
	SumActiveBps(ctx context.Context, contractID uuid.UUID) (int, error)
	// CountActiveParties returns the count of ACTIVE parties for a contract.
	CountActiveParties(ctx context.Context, contractID uuid.UUID) (int, error)
}

// MultipartySignatureStore defines persistence operations for multi-party contract signatures.
type MultipartySignatureStore interface {
	// Create inserts a new signature row.
	// Returns ErrAlreadySigned (23505) if (contract_id, signer_user_id, version) already exists.
	Create(ctx context.Context, sig *domain.MultipartyContractSignature) error
	// CountSignaturesForVersion returns the number of distinct signatures for (contract, version).
	CountSignaturesForVersion(ctx context.Context, contractID uuid.UUID, version int) (int, error)
	// ListByContractVersion returns all signatures for a (contract, version).
	ListByContractVersion(ctx context.Context, contractID uuid.UUID, version int) ([]*domain.MultipartyContractSignature, error)
}

// MultipartyTxManager runs a function inside a single Postgres transaction providing
// transaction-scoped stores for the N-party quorum check (TOCTOU-safe).
type MultipartyTxManager interface {
	WithMultipartyTx(
		ctx context.Context,
		fn func(
			ctx context.Context,
			contracts MultipartyContractStore,
			parties MultipartyPartyStore,
			sigs MultipartySignatureStore,
		) error,
	) error
}
