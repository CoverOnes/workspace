package domain

import (
	"time"

	"github.com/google/uuid"
)

// ContractActivatedEvent is published when a contract transitions to ACTIVE
// (both parties have signed the same version). DEFERRED: subscription to
// marketplace.bid_accepted will be implemented in Slice1.
type ContractActivatedEvent struct {
	EventID    uuid.UUID `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		ContractID       uuid.UUID `json:"contractId"`
		ListingID        uuid.UUID `json:"listingId"`
		AcceptedBidID    uuid.UUID `json:"acceptedBidId"`
		ClientUserID     uuid.UUID `json:"clientUserId"`
		FreelancerUserID uuid.UUID `json:"freelancerUserId"`
	} `json:"data"`
}
