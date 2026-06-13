package domain

import (
	"time"

	"github.com/google/uuid"
)

// ContractAuditLog is a single entry in the append-only hash-chain audit log.
// The chain is per-contract: each new entry hashes
//
//	SHA-256(prev_hash || contract_id || event_type || actor_id || payload)
//
// where prev_hash is the hash of the immediately preceding entry for the same
// contract_id (or the empty string for the genesis entry).
//
// All IDs are soft refs (NO FK). Referential integrity is enforced in the service layer.
type ContractAuditLog struct {
	ID         uuid.UUID      `json:"id"`
	ContractID uuid.UUID      `json:"contractId"`
	EventType  string         `json:"eventType"`
	ActorID    uuid.UUID      `json:"actorId"`
	Payload    map[string]any `json:"payload"`
	PrevHash   string         `json:"prevHash"`
	Hash       string         `json:"hash"`
	CreatedAt  time.Time      `json:"createdAt"`
}
