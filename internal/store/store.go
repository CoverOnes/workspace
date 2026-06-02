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
