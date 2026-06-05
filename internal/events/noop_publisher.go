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

// PublishMultipartyContractActivated is a no-op.
func (p *NoopPublisher) PublishMultipartyContractActivated(_ context.Context, _ *domain.MultipartyContractActivatedEvent) error {
	return nil
}

// PublishMultipartyContractCompleted is a no-op.
func (p *NoopPublisher) PublishMultipartyContractCompleted(_ context.Context, _ *domain.MultipartyContractCompletedEvent) error {
	return nil
}

// PublishMultipartyContractAddendumCreated is a no-op.
func (p *NoopPublisher) PublishMultipartyContractAddendumCreated(
	_ context.Context,
	_ *domain.MultipartyContractAddendumCreatedEvent,
) error {
	return nil
}

// PublishMultipartyContractReSigned is a no-op.
func (p *NoopPublisher) PublishMultipartyContractReSigned(_ context.Context, _ *domain.MultipartyContractReSignedEvent) error {
	return nil
}
