// Package outbox provides the in-process relay poller that reads unpublished
// entries from the event_outbox table and publishes them to Redis pub/sub,
// then marks them as published. On transport failure the entry is retried with
// exponential backoff capped at 5 minutes.
//
// Local handlers: channels that have no external Redis subscriber can register a
// local handler via Handle. When a local handler is registered for a channel, the
// poller invokes the handler directly instead of publishing to Redis. Failures are
// treated identically to publish failures (RecordFailure + exponential backoff).
// Local handlers use a longer timeout (localHandlerTimeout) to accommodate work
// that may take several seconds (e.g. PDF rendering + S3 upload).
package outbox

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/redis/go-redis/v9"
)

const (
	// pollInterval is the tick rate of the relay poller.
	pollInterval = time.Second
	// fetchLimit is the maximum number of pending outbox entries fetched per tick.
	fetchLimit = 100
	// retentionAge is the age after which published entries are deleted.
	retentionAge = 7 * 24 * time.Hour
	// staleAlertAge is the age at which unpublished entries trigger a warning.
	staleAlertAge = time.Hour
	// maxBackoff is the ceiling for exponential retry delay.
	maxBackoff = 5 * time.Minute
	// retentionInterval controls how often the janitor sweeps old rows.
	retentionInterval = 10 * time.Minute
	// localHandlerTimeout is the per-entry deadline for local (in-process) handlers.
	// Longer than the 5 s Redis timeout because local handlers may include network I/O
	// (e.g. PDF upload to the file service).
	localHandlerTimeout = 30 * time.Second
)

// EntryHandler is the signature for a local outbox entry handler.
// It receives a context with localHandlerTimeout and the outbox entry to process.
// Returning a non-nil error causes the entry to be retried with exponential backoff.
type EntryHandler func(ctx context.Context, entry *domain.OutboxEntry) error

// Publisher is a thin interface over the Redis PUBLISH command so that the
// poller can be tested without a real Redis connection.
type Publisher interface {
	// Publish sends the payload bytes to the named channel.
	Publish(ctx context.Context, channel string, payload []byte) error
}

// RedisPublisher adapts *redis.Client to the Publisher interface.
type RedisPublisher struct {
	rdb *redis.Client
}

// NewRedisPublisher wraps the Redis client for use by the poller.
func NewRedisPublisher(rdb *redis.Client) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

// Publish publishes payload to channel via Redis PUBLISH.
func (r *RedisPublisher) Publish(ctx context.Context, channel string, payload []byte) error {
	return r.rdb.Publish(ctx, channel, payload).Err()
}

// NoopPublisher discards all publish calls. Used when Redis is unavailable.
type NoopPublisher struct{}

// Publish is a no-op; it always returns nil.
func (*NoopPublisher) Publish(_ context.Context, _ string, _ []byte) error { return nil }

// Poller is the in-process transactional outbox relay. Create one with New
// and call Start in a goroutine; call Stop to drain and exit cleanly.
//
// For channels that have no external Redis subscriber, register a local handler
// with Handle before calling Start. The poller will invoke the handler directly
// for entries on that channel instead of publishing to Redis.
type Poller struct {
	outbox    store.OutboxStore
	publisher Publisher
	handlers  map[string]EntryHandler // channel → local handler; nil channels use Redis relay
	stop      chan struct{}
	done      chan struct{}
}

// New creates a Poller. outbox must be a pool-backed OutboxStore (not
// transaction-scoped). publisher handles the Redis PUBLISH for channels without
// a registered local handler.
func New(outbox store.OutboxStore, publisher Publisher) *Poller {
	return &Poller{
		outbox:    outbox,
		publisher: publisher,
		handlers:  make(map[string]EntryHandler),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Handle registers a local EntryHandler for the given channel name.
// Must be called before Start. Channels with a registered handler are processed
// in-process; all other channels are relayed to Redis as usual.
//
// Concurrency: Handle is not safe to call concurrently with Start; register all
// handlers before starting the poller.
func (p *Poller) Handle(channel string, fn EntryHandler) {
	p.handlers[channel] = fn
}

// Start runs the relay loop in the calling goroutine. It returns when Stop is
// called or ctx is canceled. Use with `go poller.Start(ctx)`.
func (p *Poller) Start(ctx context.Context) {
	defer close(p.done)

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	janitorTick := time.NewTicker(retentionInterval)
	defer janitorTick.Stop()

	slog.Info("outbox poller started")

	for {
		select {
		case <-p.stop:
			slog.Info("outbox poller stopping")
			return

		case <-ctx.Done():
			slog.Info("outbox poller context canceled")
			return

		case <-tick.C:
			p.relay(ctx)

		case <-janitorTick.C:
			p.janitor(ctx)
		}
	}
}

// Stop signals the relay loop to exit and waits until it is done.
func (p *Poller) Stop() {
	close(p.stop)
	<-p.done
}

// relay fetches one batch of pending outbox entries and attempts to publish each.
func (p *Poller) relay(ctx context.Context) {
	entries, err := p.outbox.FetchPending(ctx, fetchLimit)
	if err != nil {
		slog.Warn("outbox: fetch pending failed", "err", err)
		return
	}

	for _, e := range entries {
		p.publishEntry(ctx, e)
	}

	// Alert if any entries are unpublished and older than staleAlertAge.
	cutoff := time.Now().UTC().Add(-staleAlertAge)

	stale, err := p.outbox.CountStalePending(ctx, cutoff)
	if err != nil {
		slog.Warn("outbox: count stale pending failed", "err", err)
		return
	}

	if stale > 0 {
		slog.Warn("outbox: stale unpublished entries detected", "count", stale, "older_than", staleAlertAge)
	}
}

// publishEntry dispatches a single outbox entry.
// If a local handler is registered for e.Channel, the handler is invoked instead
// of publishing to Redis. Both paths use the same backoff-on-failure logic.
func (p *Poller) publishEntry(ctx context.Context, e *domain.OutboxEntry) {
	if handler, ok := p.handlers[e.Channel]; ok {
		p.runLocalHandler(ctx, e, handler)
		return
	}

	// Redis relay path: use a short timeout so a slow Redis does not block the whole tick.
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if pubErr := p.publisher.Publish(pubCtx, e.Channel, e.Payload); pubErr != nil {
		p.recordFailure(ctx, e, pubErr.Error())
		return
	}

	if markErr := p.outbox.MarkPublished(ctx, e.ID); markErr != nil {
		slog.Warn("outbox: mark published failed", "id", e.ID, "err", markErr)
	}
}

// runLocalHandler invokes a registered in-process handler for a local-only channel.
// Uses localHandlerTimeout to accommodate work that may include network I/O.
// On failure, the entry is retried with the same exponential backoff as Redis failures.
func (p *Poller) runLocalHandler(ctx context.Context, e *domain.OutboxEntry, handler EntryHandler) {
	handlerCtx, cancel := context.WithTimeout(ctx, localHandlerTimeout)
	defer cancel()

	if handlerErr := handler(handlerCtx, e); handlerErr != nil {
		slog.Warn("outbox: local handler failed",
			"id", e.ID,
			"channel", e.Channel,
			"attempt", e.Attempts+1,
			"err", handlerErr,
		)
		p.recordFailure(ctx, e, handlerErr.Error())

		return
	}

	if markErr := p.outbox.MarkPublished(ctx, e.ID); markErr != nil {
		slog.Warn("outbox: mark published failed", "id", e.ID, "err", markErr)
	}
}

// recordFailure logs a publish/handler failure and schedules a retry.
func (p *Poller) recordFailure(ctx context.Context, e *domain.OutboxEntry, errMsg string) {
	attempts := e.Attempts + 1
	backoff := backoffDuration(attempts)
	nextAttempt := time.Now().UTC().Add(backoff)

	slog.Warn(
		"outbox: dispatch failed",
		"id", e.ID,
		"channel", e.Channel,
		"attempt", attempts,
		"next_attempt_at", nextAttempt,
		"err", errMsg,
	)

	if recErr := p.outbox.RecordFailure(ctx, e.ID, errMsg, nextAttempt); recErr != nil {
		slog.Warn("outbox: record failure failed", "id", e.ID, "err", recErr)
	}
}

// janitor deletes published entries older than retentionAge.
func (p *Poller) janitor(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-retentionAge)

	deleted, err := p.outbox.DeleteOldPublished(ctx, cutoff)
	if err != nil {
		slog.Warn("outbox: janitor delete failed", "err", err)
		return
	}

	if deleted > 0 {
		slog.Info("outbox: janitor deleted published entries", "count", deleted)
	}
}

// backoffDuration returns min(1s * 2^(attempts-1), 5m).
func backoffDuration(attempts int) time.Duration {
	if attempts <= 0 {
		return time.Second
	}

	exp := math.Pow(2, float64(attempts-1))
	d := time.Duration(exp * float64(time.Second))

	if d > maxBackoff {
		return maxBackoff
	}

	return d
}
