package domain

import (
	"time"

	"github.com/google/uuid"
)

// ContractKind identifies the aggregate model for a contract proof.
type ContractKind string

const (
	// ContractKindBilateral is the 1:1 dual-sign contract (ContractService aggregate).
	ContractKindBilateral ContractKind = "bilateral"
	// ContractKindMultiparty is the N-vendor multi-party contract (MultipartyContractService aggregate).
	ContractKindMultiparty ContractKind = "multiparty"
)

// ContractProof is the durable legal artifact record generated when all parties
// have signed a contract. Stored as a reference to the PDF stored in the file service.
// All IDs are soft refs (NO FK). Referential integrity enforced in service layer.
type ContractProof struct {
	// ID is the primary key.
	ID uuid.UUID `json:"id"`
	// ContractID is the soft reference to the signed contract.
	ContractID uuid.UUID `json:"contractId"`
	// ContractKind distinguishes between bilateral and multiparty contract aggregates.
	ContractKind ContractKind `json:"contractKind"`
	// ContractVersion is the contract version that this proof was generated for.
	// Used for supersede detection: an existing proof at an older version is regenerated
	// when the contract moves to ACTIVE again after an addendum cycle.
	ContractVersion int `json:"contractVersion"`
	// FileID is the id returned by the file service after storing the PDF.
	FileID uuid.UUID `json:"fileId"`
	// ObjectKey is the storage object key returned by the file service.
	ObjectKey string `json:"objectKey"`
	// SHA256 is the hex-encoded SHA-256 digest of the PDF bytes, for offline verification.
	SHA256 string `json:"sha256"`
	// AuditChainHead is the tail hash of contract_audit_logs at generation time.
	// May be empty string when no audit entries exist yet (expected on initial deploy).
	AuditChainHead string `json:"auditChainHead"`
	// GeneratedAt is when the proof was generated and stored.
	GeneratedAt time.Time `json:"generatedAt"`
}

// ProofSignerEntry captures one signer's identity, role, and timestamp for embedding
// in the proof PDF. Only public-to-parties data: user id, role, signed-at timestamp.
// No IP or PII beyond what is already visible to all parties.
type ProofSignerEntry struct {
	// UserID is the signer's user identifier.
	UserID uuid.UUID
	// Role is a human-readable label (e.g. "CLIENT", "FREELANCER", or a role name).
	Role string
	// SignedAt is the UTC timestamp when the party submitted their signature.
	SignedAt time.Time
}

// ProofDocument carries all the data needed to render the proof PDF.
// It is a pure value type; rendering is done by the proof_pdf package.
type ProofDocument struct {
	// ContractID uniquely identifies the contract this proof covers.
	ContractID uuid.UUID
	// ContractKind is "bilateral" or "multiparty".
	ContractKind ContractKind
	// Title is the human-readable contract title.
	Title string
	// TermsSummary is a truncated/sanitized representation of the contract terms.
	TermsSummary string
	// Version is the contract version that was signed.
	Version int
	// Signers is the ordered list of all party signatures.
	Signers []ProofSignerEntry
	// AuditChainHead is the tail hash at proof generation time (may be empty).
	AuditChainHead string
	// GeneratedAt is the UTC time the proof was generated.
	GeneratedAt time.Time
}
