// Package events provides event publishing for the workspace service.
package events

import (
	"context"

	"github.com/CoverOnes/workspace/internal/domain"
)

// Publisher publishes domain events to a transport (Redis pub/sub).
// Implementations must be safe for concurrent use.
type Publisher interface {
	// PublishContractActivated sends the workspace.contract_activated event.
	// Best-effort: callers MUST NOT treat a publish failure as a reason to
	// roll back the sign transaction. The contract row is the authoritative record.
	PublishContractActivated(ctx context.Context, evt *domain.ContractActivatedEvent) error
}
