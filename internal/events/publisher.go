// Package events provides event publishing for the workspace service.
package events

import (
	"context"

	"github.com/CoverOnes/workspace/internal/domain"
)

// Publisher publishes domain events to a transport (Redis pub/sub).
// Implementations must be safe for concurrent use.
type Publisher interface {
	// PublishContractActivated sends the workspace.contract_activated event
	// for the 1:1 dual-sign contract aggregate.
	// Best-effort: callers MUST NOT treat a publish failure as a reason to
	// roll back the sign transaction. The contract row is the authoritative record.
	PublishContractActivated(ctx context.Context, evt *domain.ContractActivatedEvent) error

	// PublishMultipartyContractActivated sends the workspace.contract_activated event
	// for the multi-party N-vendor contract aggregate (§14 dotted-lowercase channel).
	// Best-effort: same semantics as PublishContractActivated.
	PublishMultipartyContractActivated(ctx context.Context, evt *domain.MultipartyContractActivatedEvent) error

	// PublishMultipartyContractCompleted sends the workspace.contract_completed event
	// when a multiparty contract milestone is marked COMPLETED.
	// Best-effort: callers MUST NOT roll back the milestone completion on publish failure.
	PublishMultipartyContractCompleted(ctx context.Context, evt *domain.MultipartyContractCompletedEvent) error
}
