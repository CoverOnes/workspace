// Package store defines the storage interfaces for the workspace domain.
package store

import (
	"context"
	"time"

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
	// GetActivePartyByVendor returns the ACTIVE party row for (contractID, vendorUserID).
	// Returns ErrNotParty if no ACTIVE row exists (the vendor is not a current member).
	// Used by signTx to enforce party-membership authz before creating a signature.
	GetActivePartyByVendor(ctx context.Context, contractID, vendorUserID uuid.UUID) (*domain.MultipartyContractParty, error)
	// GetActivePartyByID returns the ACTIVE party row by its primary key.
	// Returns ErrNotParty if no ACTIVE row exists.
	GetActivePartyByID(ctx context.Context, partyID uuid.UUID) (*domain.MultipartyContractParty, error)
	// UpdatePartyShare updates share_bps for an ACTIVE party row.
	// Returns ErrNotParty if the party does not exist or is not ACTIVE.
	UpdatePartyShare(ctx context.Context, contractID, partyID uuid.UUID, newShareBps int) (*domain.MultipartyContractParty, error)
	// ListActiveByContract returns all ACTIVE parties for a contract.
	ListActiveByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.MultipartyContractParty, error)
	// SumActiveBps returns the sum of share_bps for all ACTIVE parties of a contract.
	SumActiveBps(ctx context.Context, contractID uuid.UUID) (int, error)
	// CountActiveParties returns the count of ACTIVE parties for a contract.
	CountActiveParties(ctx context.Context, contractID uuid.UUID) (int, error)
}

// AddendumStore defines persistence operations for contract addenda.
// Addenda record each addendum event: which party was added, by whom, and which
// version transition it represents. No FK — all IDs are soft references.
type AddendumStore interface {
	// Create inserts a new addendum row.
	Create(ctx context.Context, a *domain.ContractAddendum) error
	// ListByContract returns all addenda for a contract ordered by created_at ASC.
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAddendum, error)
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
// The addenda AddendumStore is the 4th arg — callers that do not use it may ignore it
// with `_ store.AddendumStore`.
type MultipartyTxManager interface {
	WithMultipartyTx(
		ctx context.Context,
		fn func(
			ctx context.Context,
			contracts MultipartyContractStore,
			parties MultipartyPartyStore,
			sigs MultipartySignatureStore,
			addenda AddendumStore,
		) error,
	) error
}

// MilestoneTxManager runs a function inside a single Postgres transaction that
// provides a transaction-scoped MultipartyContractStore (for GetByIDForUpdate) and
// MilestoneStore (for MarkCompleted). Used by CompleteMilestone to prevent a
// concurrent CancelContract from racing between the ACTIVE-status guard and the
// milestone write.
type MilestoneTxManager interface {
	WithMilestoneTx(
		ctx context.Context,
		fn func(ctx context.Context, contracts MultipartyContractStore, milestones MilestoneStore) error,
	) error
}

// MilestoneStore defines persistence operations for multiparty contract milestones.
type MilestoneStore interface {
	// Create inserts a new milestone row.
	Create(ctx context.Context, m *domain.Milestone) error
	// GetByID fetches a milestone by primary key.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Milestone, error)
	// ListByContract returns all milestones for a multiparty contract ordered by sequence ASC, created_at ASC.
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Milestone, error)
	// MarkCompleted sets status=COMPLETED and completed_at for the given milestone.
	// Returns ErrMilestoneNotFound if the row does not exist.
	// Returns ErrMilestoneAlreadyDone if it is already COMPLETED.
	MarkCompleted(ctx context.Context, id uuid.UUID, completedAt time.Time) (*domain.Milestone, error)
}
