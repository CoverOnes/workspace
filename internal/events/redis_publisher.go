package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/redis/go-redis/v9"
)

const (
	channelContractActivated           = "workspace.contract_activated"
	channelMultipartyContractActivated = "workspace.contract_activated"
	channelContractCompleted           = "workspace.contract_completed"
)

// RedisPublisher publishes events to Redis pub/sub channels.
type RedisPublisher struct {
	rdb *redis.Client
}

// NewRedisPublisher returns a RedisPublisher backed by the given Redis client.
func NewRedisPublisher(rdb *redis.Client) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

// PublishContractActivated serializes the event and publishes it to Redis.
// Transport failures are returned to the caller (caller should log and continue —
// the contract row is the durable source of truth).
func (p *RedisPublisher) PublishContractActivated(ctx context.Context, evt *domain.ContractActivatedEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal contract_activated event: %w", err)
	}

	if err := p.rdb.Publish(ctx, channelContractActivated, payload).Err(); err != nil {
		return fmt.Errorf("redis publish %s: %w", channelContractActivated, err)
	}

	return nil
}

// PublishMultipartyContractActivated serializes the event and publishes it to Redis
// on the same §14 workspace.contract_activated channel.
// Transport failures are returned to the caller (caller should log and continue).
func (p *RedisPublisher) PublishMultipartyContractActivated(ctx context.Context, evt *domain.MultipartyContractActivatedEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal multiparty contract_activated event: %w", err)
	}

	if err := p.rdb.Publish(ctx, channelMultipartyContractActivated, payload).Err(); err != nil {
		return fmt.Errorf("redis publish %s: %w", channelMultipartyContractActivated, err)
	}

	return nil
}

// PublishMultipartyContractCompleted serializes the event and publishes it to Redis
// on the §14 workspace.contract_completed channel.
// Transport failures are returned to the caller (caller should log and continue —
// the milestone row is the durable source of truth; publish is best-effort).
func (p *RedisPublisher) PublishMultipartyContractCompleted(ctx context.Context, evt *domain.MultipartyContractCompletedEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal contract_completed event: %w", err)
	}

	if err := p.rdb.Publish(ctx, channelContractCompleted, payload).Err(); err != nil {
		return fmt.Errorf("redis publish %s: %w", channelContractCompleted, err)
	}

	return nil
}
