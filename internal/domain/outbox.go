package domain

import (
	"time"

	"github.com/google/uuid"
)

// OutboxEntry represents a row in the event_outbox table.
// It is created atomically with the domain write (same transaction) and
// consumed by the in-process poller, which relays it to the event bus.
type OutboxEntry struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventID       uuid.UUID // consumer-facing dedup key
	Channel       string
	Payload       []byte // exact bytes; some events are signed and need byte-for-byte round-trip
	CreatedAt     time.Time
	PublishedAt   *time.Time // nil = pending
	Attempts      int
	LastError     *string
	NextAttemptAt time.Time
}
