package domain

import (
	"time"

	"github.com/google/uuid"
)

// MultipartyContractStatus represents the lifecycle state of a multi-party contract.
type MultipartyContractStatus string

const (
	MultipartyContractStatusDraft             MultipartyContractStatus = "DRAFT"
	MultipartyContractStatusPendingSignatures MultipartyContractStatus = "PENDING_SIGNATURES"
	MultipartyContractStatusActive            MultipartyContractStatus = "ACTIVE"
	// MultipartyContractStatusAddendumPending is set when a new party is added to an ACTIVE
	// contract. The owner must update all shares (Σ=10000) and re-submit before parties
	// can re-sign the new digest. This is the Phase-4 "add-then-resubmit + re-sign" state.
	MultipartyContractStatusAddendumPending MultipartyContractStatus = "ADDENDUM_PENDING"
	MultipartyContractStatusCompleted       MultipartyContractStatus = "COMPLETED"
	// MultipartyContractStatusCancelled matches the SQL CHECK constraint spelling.
	MultipartyContractStatusCancelled MultipartyContractStatus = "CANCELLED" //nolint:misspell // matches SQL CHECK constraint
)

// MultipartyContract is the aggregate root for a multi-party N-vendor contract.
// All party / tender references are soft refs (NO FK). Referential integrity is
// enforced in the service layer.
type MultipartyContract struct {
	ID          uuid.UUID                `json:"id"`
	TenderID    uuid.UUID                `json:"tenderId"`
	Status      MultipartyContractStatus `json:"status"`
	ContentHash string                   `json:"contentHash"`
	Version     int                      `json:"version"`
	// PartyCount is frozen at SubmitForSignatures time (= count of ACTIVE parties when
	// submit is called). The quorum check uses this frozen value — NOT a live COUNT(*) —
	// so that a roster shrink after submit cannot manipulate the activation threshold.
	PartyCount int     `json:"partyCount"`
	Currency   *string `json:"currency,omitempty"`
	// PosterUserID is the tender owner who may manage milestones (add/complete).
	// Nullable: rows created before migration 000007 will have nil.
	PosterUserID *uuid.UUID `json:"posterUserId,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}

// MultipartyPartyStatus represents the status of a party within a multi-party contract.
type MultipartyPartyStatus string

const (
	MultipartyPartyStatusActive   MultipartyPartyStatus = "ACTIVE"
	MultipartyPartyStatusExited   MultipartyPartyStatus = "EXITED"
	MultipartyPartyStatusReplaced MultipartyPartyStatus = "REPLACED"
)

// MultipartyContractParty records one vendor's membership in a multi-party contract.
// share_bps is in basis points (0–10000). The sum of all ACTIVE parties' share_bps
// must equal exactly 10000 before the contract can be submitted for signatures.
type MultipartyContractParty struct {
	ID           uuid.UUID             `json:"id"`
	ContractID   uuid.UUID             `json:"contractId"`
	VendorUserID uuid.UUID             `json:"vendorUserId"`
	RoleID       *uuid.UUID            `json:"roleId,omitempty"`
	ShareBps     int                   `json:"shareBps"`
	Status       MultipartyPartyStatus `json:"status"`
	CreatedAt    time.Time             `json:"createdAt"`
	UpdatedAt    time.Time             `json:"updatedAt"`
}

// MultipartyContractSignature is an append-only audit record for a party signing
// a specific version of a multi-party contract.
type MultipartyContractSignature struct {
	ID                uuid.UUID `json:"id"`
	ContractID        uuid.UUID `json:"contractId"`
	SignerUserID      uuid.UUID `json:"signerUserId"`
	Version           int       `json:"version"`
	SignedContentHash string    `json:"signedContentHash"`
	SignedAt          time.Time `json:"signedAt"`
	CreatedAt         time.Time `json:"createdAt"`
}

// ContractAddendum records one addendum event: a new party was added to an ACTIVE
// contract, causing a version bump and a transition to ADDENDUM_PENDING.
// No FK — contract_id, new_party_id, new_vendor_user_id, triggered_by are soft refs.
type ContractAddendum struct {
	ID              uuid.UUID `json:"id"`
	ContractID      uuid.UUID `json:"contractId"`
	FromVersion     int       `json:"fromVersion"`
	ToVersion       int       `json:"toVersion"`
	NewPartyID      uuid.UUID `json:"newPartyId"`
	NewVendorUserID uuid.UUID `json:"newVendorUserId"`
	TriggeredBy     uuid.UUID `json:"triggeredBy"`
	CreatedAt       time.Time `json:"createdAt"`
}

// MultipartyContractActivatedEvent is published (§14 dotted-lowercase channel
// workspace.contract_activated) when a multi-party contract reaches quorum.
type MultipartyContractActivatedEvent struct {
	EventID    uuid.UUID `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		ContractID uuid.UUID `json:"contractId"`
		TenderID   uuid.UUID `json:"tenderId"`
		PartyCount int       `json:"partyCount"`
	} `json:"data"`
}

// MultipartyContractAddendumCreatedEvent is published (workspace.contract_addendum_created)
// when a new party is added to an ACTIVE contract and the contract transitions to ADDENDUM_PENDING.
type MultipartyContractAddendumCreatedEvent struct {
	EventID    uuid.UUID `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		ContractID      uuid.UUID `json:"contractId"`
		TenderID        uuid.UUID `json:"tenderId"`
		FromVersion     int       `json:"fromVersion"`
		ToVersion       int       `json:"toVersion"`
		NewVendorUserID uuid.UUID `json:"newVendorUserId"`
		PartyCount      int       `json:"partyCount"`
	} `json:"data"`
}

// MultipartyContractReSignedEvent is published (workspace.contract_re_signed)
// when all ACTIVE parties have re-signed after an addendum and the contract returns to ACTIVE.
type MultipartyContractReSignedEvent struct {
	EventID    uuid.UUID `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		ContractID uuid.UUID `json:"contractId"`
		TenderID   uuid.UUID `json:"tenderId"`
		NewVersion int       `json:"newVersion"`
		PartyCount int       `json:"partyCount"`
	} `json:"data"`
}

// ValidMultipartyContractTransition returns true when the contract can transition
// from its current status to the requested target status.
func ValidMultipartyContractTransition(from, to MultipartyContractStatus) bool {
	if from == to {
		return false
	}

	switch from {
	case MultipartyContractStatusDraft:
		switch to {
		case MultipartyContractStatusPendingSignatures, MultipartyContractStatusCancelled:
			return true
		}

	case MultipartyContractStatusPendingSignatures:
		switch to {
		case MultipartyContractStatusActive, MultipartyContractStatusCancelled:
			return true
		}

	case MultipartyContractStatusActive:
		switch to {
		case MultipartyContractStatusAddendumPending, MultipartyContractStatusCompleted, MultipartyContractStatusCancelled:
			return true
		}

	case MultipartyContractStatusAddendumPending:
		switch to {
		case MultipartyContractStatusPendingSignatures, MultipartyContractStatusCancelled:
			return true
		}
	}

	// COMPLETED and CANCELLED are terminal states — no further transitions.
	//nolint:misspell // "CANCELLED" matches the SQL CHECK constraint spelling used in migrations
	return false
}
