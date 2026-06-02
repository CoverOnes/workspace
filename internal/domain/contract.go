package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ContractStatus represents the lifecycle state of a contract.
type ContractStatus string

const (
	ContractStatusDraft            ContractStatus = "DRAFT"
	ContractStatusPendingSignature ContractStatus = "PENDING_SIGNATURE"
	ContractStatusSigned           ContractStatus = "SIGNED"
	ContractStatusActive           ContractStatus = "ACTIVE"
	ContractStatusCompleted        ContractStatus = "COMPLETED"
	// ContractStatusCanceled maps to the British-spelled value in the SQL CHECK constraint
	// (migration 000001). The misspell linter suppression is intentional — DO NOT change.
	ContractStatusCanceled ContractStatus = "CANCELLED" //nolint:misspell // matches SQL CHECK constraint
)

// Contract is the aggregate root for post-deal collaboration.
// All party IDs are soft refs (NO FK). Referential integrity enforced in service layer.
type Contract struct {
	ID               uuid.UUID       `json:"id"`
	ListingID        uuid.UUID       `json:"listingId"`
	AcceptedBidID    uuid.UUID       `json:"acceptedBidId"`
	ClientUserID     uuid.UUID       `json:"clientUserId"`
	FreelancerUserID uuid.UUID       `json:"freelancerUserId"`
	Title            string          `json:"title"`
	Terms            string          `json:"terms"`
	Amount           decimal.Decimal `json:"amount"`
	Currency         string          `json:"currency"`
	ContentHash      string          `json:"contentHash"`
	Version          int             `json:"version"`
	Status           ContractStatus  `json:"status"`
	ActivatedAt      *time.Time      `json:"activatedAt,omitempty"`
	CompletedAt      *time.Time      `json:"completedAt,omitempty"`
	DeletedAt        *time.Time      `json:"deletedAt,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
}
