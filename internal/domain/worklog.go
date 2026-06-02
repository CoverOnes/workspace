package domain

import (
	"time"

	"github.com/google/uuid"
)

// Worklog is a work-time record hanging off a contract.
// Authored by a contract party. Append-style; soft-deletable by author.
// minutes is the canonical duration (hours = minutes/60 derived in API response).
type Worklog struct {
	ID          uuid.UUID  `json:"id"`
	ContractID  uuid.UUID  `json:"contractId"`
	UserID      uuid.UUID  `json:"userId"`
	Description string     `json:"description"`
	Minutes     int        `json:"minutes"`
	LoggedAt    time.Time  `json:"loggedAt"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}
