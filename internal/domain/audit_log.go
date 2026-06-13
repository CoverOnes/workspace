package domain

import (
	"time"

	"github.com/google/uuid"
)

// ContractAuditLog is a single entry in the append-only hash-chain audit log.
//
// # Security scope
//
// The chain detects accidental or application-layer tampering: reordering, deletion,
// payload modification, and partial writes. Each entry's hash covers the previous
// entry's hash so any break in the chain is detectable by VerifyAuditChain.
//
// Limitation: a DB-privileged attacker who can write arbitrary rows can recompute the
// entire chain and produce a consistent but forged history. Preventing that requires
// an HMAC key that is never stored in the DB (e.g. an HSM or external audit anchor).
// Whether to add HMAC is a separate product decision tracked in the GTD backlog.
//
// All IDs are soft refs (NO FK). Referential integrity is enforced in the service layer.
type ContractAuditLog struct {
	Seq        int64          `json:"seq"`
	ID         uuid.UUID      `json:"id"`
	ContractID uuid.UUID      `json:"contractId"`
	EventType  string         `json:"eventType"`
	ActorID    uuid.UUID      `json:"actorId"`
	Payload    map[string]any `json:"payload"`
	PrevHash   string         `json:"prevHash"`
	Hash       string         `json:"hash"`
	CreatedAt  time.Time      `json:"createdAt"`
}
