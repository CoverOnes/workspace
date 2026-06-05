package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// MilestoneStatus represents the lifecycle state of a multiparty contract milestone.
type MilestoneStatus string

const (
	MilestoneStatusPending   MilestoneStatus = "PENDING"
	MilestoneStatusCompleted MilestoneStatus = "COMPLETED"
)

// Milestone is a named payment checkpoint on a multiparty contract.
// Completing a milestone triggers a disburse of Milestone.Amount to the payment service.
//
// Sum invariant: milestone amounts are NOT required to equal any contract total —
// the poster adds milestones incrementally. The payment service independently
// re-checks the sum at settlement-plan creation time.
type Milestone struct {
	ID              uuid.UUID       `json:"id"`
	MultiContractID uuid.UUID       `json:"multiContractId"`
	Name            string          `json:"name"`
	Amount          decimal.Decimal `json:"amount"`
	Currency        string          `json:"currency"`
	Sequence        int             `json:"sequence"`
	Status          MilestoneStatus `json:"status"`
	CompletedAt     *time.Time      `json:"completedAt,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

// MultipartyContractCompletedEvent is published (§14 dotted-lowercase channel
// workspace.contract_completed) when a milestone is marked COMPLETED.
// Payment subscribes to this event to trigger per-milestone disbursement.
type MultipartyContractCompletedEvent struct {
	EventID    uuid.UUID `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		ContractID  uuid.UUID       `json:"contractId"`
		TenderID    uuid.UUID       `json:"tenderId"`
		MilestoneID uuid.UUID       `json:"milestoneId"`
		Amount      decimal.Decimal `json:"amount"`
		Currency    string          `json:"currency"`
	} `json:"data"`
}
