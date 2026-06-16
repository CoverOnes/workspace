package domain

import (
	"time"

	"github.com/google/uuid"
)

// SignerRole represents whether the signer is the client or freelancer party.
type SignerRole string

const (
	SignerRoleClient     SignerRole = "CLIENT"
	SignerRoleFreelancer SignerRole = "FREELANCER"
)

// Signature is an append-only audit record for a party signing a specific contract version.
// Records the signer, the content_hash they signed, IP, user-agent, and signed_at.
// Immutable after creation — invalidation is by version, not deletion.
type Signature struct {
	ID                uuid.UUID  `json:"id"`
	ContractID        uuid.UUID  `json:"contractId"`
	SignerUserID      uuid.UUID  `json:"signerUserId"`
	SignerRole        SignerRole `json:"signerRole"`
	ContractVersion   int        `json:"contractVersion"`
	SignedContentHash string     `json:"signedContentHash"`
	SignerIP          *string    `json:"signerIp,omitempty"`
	UserAgent         *string    `json:"userAgent,omitempty"`
	SignedAt          time.Time  `json:"signedAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	// FileID is an optional reference to an attachment registered with the file service.
	// nil when no document is attached to this signature.
	FileID *uuid.UUID `json:"fileId,omitempty"`
}
