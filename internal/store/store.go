// Package store defines the storage interfaces for the workspace domain.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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
	// GetByID fetches a signature by its primary key.
	// Returns ErrSignatureNotFound when no row exists.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Signature, error)
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.Signature, error)
	// CountValidSignatures returns the count of distinct valid signatures for the
	// current (contractID, version, contentHash) combination.
	CountValidSignatures(ctx context.Context, contractID uuid.UUID, version int, contentHash string) (int, error)
	// SetFileID persists the file_id for an existing signature row.
	// Used after a successful S2S register call to record the attachment.
	SetFileID(ctx context.Context, id, fileID uuid.UUID) error
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
// The outbox OutboxStore is included so callers can enqueue events atomically
// with domain writes (transactional outbox pattern).
type TxManager interface {
	WithTx(
		ctx context.Context,
		fn func(ctx context.Context, contracts ContractStore, signatures SignatureStore, outbox OutboxStore) error,
	) error
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
// with `_ store.AddendumStore`. The outbox OutboxStore (5th arg) is provided so
// callers can enqueue events atomically with domain writes.
type MultipartyTxManager interface {
	WithMultipartyTx(
		ctx context.Context,
		fn func(
			ctx context.Context,
			contracts MultipartyContractStore,
			parties MultipartyPartyStore,
			sigs MultipartySignatureStore,
			addenda AddendumStore,
			outbox OutboxStore,
		) error,
	) error
}

// MilestoneTxManager runs a function inside a single Postgres transaction that
// provides a transaction-scoped MultipartyContractStore (for GetByIDForUpdate) and
// MilestoneStore (for MarkCompleted). Used by CompleteMilestone to prevent a
// concurrent CancelContract from racing between the ACTIVE-status guard and the
// milestone write. The outbox OutboxStore is included so callers can enqueue
// events atomically with the milestone write.
type MilestoneTxManager interface {
	WithMilestoneTx(
		ctx context.Context,
		fn func(ctx context.Context, contracts MultipartyContractStore, milestones MilestoneStore, outbox OutboxStore) error,
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
	// SumAmountsByContract returns the sum of ALL milestone amounts for the contract
	// (no status filter — includes PENDING and COMPLETED milestones).
	// Returns decimal.Zero when no milestones exist (no error).
	// Used by the S2S escrow-cap endpoint consumed by the payment service.
	SumAmountsByContract(ctx context.Context, contractID uuid.UUID) (decimal.Decimal, error)
}

// OutboxEnqueueInput carries the fields needed to insert an outbox row.
// It is used by all three tx managers so that the domain write and the
// outbox INSERT happen atomically in the same transaction.
type OutboxEnqueueInput struct {
	AggregateType string
	AggregateID   uuid.UUID
	EventID       uuid.UUID
	Channel       string
	Payload       []byte
}

// OutboxStore defines persistence operations for the event_outbox table.
// Enqueue is used inside transactions (atomically with domain writes).
// The poller uses the pool-backed implementation for the polling queries.
type OutboxStore interface {
	// Enqueue inserts a new outbox row. Must be called inside an active transaction.
	Enqueue(ctx context.Context, in *OutboxEnqueueInput) error
	// FetchPending fetches up to limit rows WHERE published_at IS NULL AND
	// next_attempt_at <= now() ORDER BY created_at LIMIT limit FOR UPDATE SKIP LOCKED.
	// Multi-replica safe: SKIP LOCKED prevents double-processing.
	FetchPending(ctx context.Context, limit int) ([]*domain.OutboxEntry, error)
	// MarkPublished sets published_at = now() for the given row.
	MarkPublished(ctx context.Context, id uuid.UUID) error
	// RecordFailure increments attempts, sets last_error and next_attempt_at (exponential backoff).
	RecordFailure(ctx context.Context, id uuid.UUID, lastErr string, nextAttemptAt time.Time) error
	// DeleteOldPublished deletes rows WHERE published_at < cutoff.
	// Unpublished rows are never deleted.
	DeleteOldPublished(ctx context.Context, cutoff time.Time) (int64, error)
	// CountStalePending counts rows WHERE published_at IS NULL AND created_at < cutoff.
	// Used by the alerting check (stale unpublished > 1h threshold).
	CountStalePending(ctx context.Context, cutoff time.Time) (int64, error)
}

// AuditAppendInput carries the caller-supplied fields for a new audit log entry.
// The store is responsible for acquiring the advisory lock, reading the chain tail,
// computing prev_hash and hash, and inserting — all inside a single transaction.
type AuditAppendInput struct {
	ContractID uuid.UUID
	EventType  string
	ActorID    uuid.UUID
	Payload    map[string]any
}

// ContractAuditLogStore defines persistence operations for contract audit logs.
// This is an append-only store — no Update or Delete methods are intentionally exposed.
// The advisory lock (pg_advisory_xact_lock) is acquired inside Append to serialize
// concurrent writes for the same contract_id, preventing hash-chain forks.
type ContractAuditLogStore interface {
	// Append acquires a tx-level advisory lock on contract_id, reads the chain tail,
	// computes prev_hash and hash, then inserts the new entry — all in one transaction.
	// This eliminates the TOCTOU window that would allow concurrent appends to fork
	// the chain.
	Append(ctx context.Context, in *AuditAppendInput) (*domain.ContractAuditLog, error)
	// ListByContract returns all audit log entries for a contract ordered by seq ASC.
	ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAuditLog, error)
	// TailHash returns the hash of the most-recent audit log entry for contractID,
	// or "" when no entries exist (genesis / no audit log yet).
	// This is a non-locking point-in-time snapshot read — suitable for proof generation
	// where locking the chain is not required.
	TailHash(ctx context.Context, contractID uuid.UUID) (string, error)
}

// ContractProofStore defines persistence operations for contract proof records.
// Proofs are durable legal artifacts — no soft-delete or TTL operations are exposed.
type ContractProofStore interface {
	// Create inserts a new proof row.
	// Returns ErrProofAlreadyExists (mapped from UNIQUE violation) when a proof already
	// exists for (contract_id, contract_kind) — enabling idempotent generation.
	Create(ctx context.Context, p *domain.ContractProof) error
	// GetByContract fetches the proof for (contractID, kind).
	// Returns ErrProofNotFound when no proof has been generated yet.
	GetByContract(ctx context.Context, contractID uuid.UUID, kind domain.ContractKind) (*domain.ContractProof, error)
	// Supersede replaces an existing proof row for (contractID, kind) in place with new
	// content (new file_id, object_key, sha256, audit_chain_head, contract_version, generated_at).
	// Used when an addendum re-sign produces a new ACTIVE version that supersedes v1.
	// Returns ErrProofNotFound when no existing row exists to supersede.
	Supersede(ctx context.Context, p *domain.ContractProof) error
}

// ErrProofNotFound is returned by ContractProofStore.GetByContract when no proof exists.
var ErrProofNotFound = errors.New("contract proof not found")

// ErrProofAlreadyExists is returned by ContractProofStore.Create when a proof for
// (contract_id, contract_kind) already exists (idempotency guard).
var ErrProofAlreadyExists = errors.New("contract proof already exists")
