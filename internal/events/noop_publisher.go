package events

import (
	"context"

	"github.com/CoverOnes/workspace/internal/domain"
)

// NoopPublisher discards all events. Used when Redis is not configured.
type NoopPublisher struct{}

// NewNoopPublisher returns a NoopPublisher.
func NewNoopPublisher() *NoopPublisher {
	return &NoopPublisher{}
}

// PublishContractActivated is a no-op.
func (p *NoopPublisher) PublishContractActivated(_ context.Context, _ *domain.ContractActivatedEvent) error {
	return nil
}
