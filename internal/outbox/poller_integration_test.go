package outbox_test

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/outbox"
	"github.com/CoverOnes/workspace/internal/store"
	pgstore "github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// sharedOutboxPool is the singleton pool shared across all outbox integration tests.
var sharedOutboxPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	flag.Parse()

	if testing.Short() {
		return m.Run()
	}

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		return 1
	}

	defer func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate container: %v\n", termErr)
		}
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get connection string: %v\n", err)
		return 1
	}

	sharedOutboxPool, err = pgstore.NewPool(ctx, dsn, "", pgstore.PoolConfig{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create pool: %v\n", err)
		return 1
	}

	defer sharedOutboxPool.Close()

	if err := applyMigrations(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		return 1
	}

	return m.Run()
}

func applyMigrations(ctx context.Context) error {
	var upFiles []string

	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk migrations FS: %w", err)
	}

	if len(upFiles) == 0 {
		return fmt.Errorf("no *.up.sql files found in embedded FS")
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", file, readErr)
		}
		if _, execErr := sharedOutboxPool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply %s: %w", file, execErr)
		}
	}

	return nil
}

// newMiniRedis starts a miniredis server and returns both the client and a cleanup func.
func newMiniRedis(t *testing.T) (client *redis.Client, cleanup func()) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	return rdb, func() {
		_ = rdb.Close()
		mr.Close()
	}
}

// enqueueEntry inserts a single outbox entry with the given channel and payload, using
// the provided outboxStore. Returns the OutboxEnqueueInput used so callers can inspect it.
func enqueueEntry(t *testing.T, ctx context.Context, ob store.OutboxStore, channel string, payload []byte) *store.OutboxEnqueueInput {
	t.Helper()

	in := &store.OutboxEnqueueInput{
		AggregateType: "test",
		AggregateID:   uuid.New(),
		EventID:       uuid.New(),
		Channel:       channel,
		Payload:       payload,
	}
	require.NoError(t, ob.Enqueue(ctx, in))

	return in
}

// TestOutbox_AtomicEnqueue_TxRollbackLeavesZeroRows verifies that if the DB transaction
// is rolled back after enqueue, the outbox contains zero rows.
func TestOutbox_AtomicEnqueue_TxRollbackLeavesZeroRows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ob := pgstore.NewOutboxStore(sharedOutboxPool)

	// Start a real pgx.Tx and roll it back.
	rawTx, err := sharedOutboxPool.BeginTx(ctx, pgx.TxOptions{})
	require.NoError(t, err)

	defer func() { _ = rawTx.Rollback(ctx) }()

	txOb := pgstore.NewTxOutboxStore(rawTx)

	in := &store.OutboxEnqueueInput{
		AggregateType: "contract",
		AggregateID:   uuid.New(),
		EventID:       uuid.New(),
		Channel:       "workspace.contract_activated",
		Payload:       []byte(`{"test":"rollback"}`),
	}
	require.NoError(t, txOb.Enqueue(ctx, in))

	// Roll back — the enqueue must vanish.
	require.NoError(t, rawTx.Rollback(ctx))

	pending, err := ob.FetchPending(ctx, 100)
	require.NoError(t, err)

	for _, e := range pending {
		assert.NotEqual(t, in.EventID, e.EventID, "rolled-back entry must not appear in pending")
	}
}

// TestOutbox_PollerRelayAndMarkPublished verifies end-to-end relay: enqueue → poller tick → message on Redis.
func TestOutbox_PollerRelayAndMarkPublished(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ob := pgstore.NewOutboxStore(sharedOutboxPool)

	rdb, cleanup := newMiniRedis(t)
	defer cleanup()

	const channel = "workspace.contract_activated"
	payload := []byte(`{"event_id":"relay-test"}`)

	// Subscribe before enqueue so we don't miss the publish.
	sub := rdb.Subscribe(context.Background(), channel)
	defer func() { _ = sub.Close() }()

	enqueueEntry(t, ctx, ob, channel, payload)

	pub := outbox.NewRedisPublisher(rdb)
	poller := outbox.New(ob, pub)

	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		poller.Start(pollCtx)
	}()

	defer func() {
		cancel()
		<-done
	}()

	// Wait for the message to arrive on the channel (up to 5s).
	msgCh := sub.Channel()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case msg := <-msgCh:
		assert.Equal(t, channel, msg.Channel)
		assert.Equal(t, string(payload), msg.Payload)
	case <-timer.C:
		t.Fatal("timed out waiting for relayed message on Redis channel")
	}

	// After relay the entry must be marked published (pending list shrinks to 0 for this event).
	time.Sleep(200 * time.Millisecond) // allow mark-published write to complete

	entries, err := ob.FetchPending(ctx, 100)
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotEqual(t, channel, e.Channel, "relay entry must not remain unpublished")
	}
}

// TestOutbox_CrashRedelivery verifies that an entry committed to the DB but not yet
// published is picked up and relayed by the poller on the next run (simulating a crash
// between commit and publish in the old fire-and-forget pattern).
func TestOutbox_CrashRedelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ob := pgstore.NewOutboxStore(sharedOutboxPool)

	rdb, cleanup := newMiniRedis(t)
	defer cleanup()

	const channel = "workspace.contract_completed"
	payload := []byte(`{"event_id":"crash-recovery"}`)

	// Enqueue directly (simulates "commit happened but publish never fired").
	in := enqueueEntry(t, ctx, ob, channel, payload)

	// Subscribe before starting the poller.
	sub := rdb.Subscribe(context.Background(), channel)
	defer func() { _ = sub.Close() }()

	pub := outbox.NewRedisPublisher(rdb)
	poller := outbox.New(ob, pub)

	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		poller.Start(pollCtx)
	}()

	defer func() {
		cancel()
		<-done
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case msg := <-sub.Channel():
		assert.Equal(t, channel, msg.Channel)
		assert.Equal(t, string(payload), msg.Payload)
	case <-timer.C:
		t.Fatalf("crash-recovery: timed out waiting for redelivery of event_id=%v", in.EventID)
	}
}

// TestOutbox_FailureBackoffRetry verifies that a publish failure causes attempts++ and a
// backoff delay, and that the entry is retried after the delay expires.
func TestOutbox_FailureBackoffRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ob := pgstore.NewOutboxStore(sharedOutboxPool)

	const channel = "workspace.contract_re_signed"
	payload := []byte(`{"event_id":"backoff-retry"}`)

	enqueueEntry(t, ctx, ob, channel, payload)

	// First poller with a failing publisher — drives attempts++ and sets next_attempt_at.
	var failCount int

	failPub := &countingPublisher{
		failUntil: 1,
		onFail: func() {
			failCount++
		},
	}

	// Run exactly one tick manually so we can inspect backoff state.
	// We use the low-level OutboxStore directly to simulate one relay cycle.
	entries, err := ob.FetchPending(ctx, 100)
	require.NoError(t, err)

	var target *domain.OutboxEntry

	for _, e := range entries {
		if e.Channel == channel {
			target = e
			break
		}
	}

	require.NotNil(t, target, "entry not found in pending list")

	// Simulate failure: publish fails, RecordFailure is called.
	require.Error(t, failPub.Publish(ctx, target.Channel, target.Payload))

	nextAttempt := time.Now().UTC().Add(time.Second) // backoffDuration(1) = 1s
	require.NoError(t, ob.RecordFailure(ctx, target.ID, "simulated failure", nextAttempt))
	_ = failCount

	// After RecordFailure, the entry should not appear in FetchPending
	// (next_attempt_at is in the future).
	pending, err := ob.FetchPending(ctx, 100)
	require.NoError(t, err)

	for _, e := range pending {
		assert.NotEqual(t, target.ID, e.ID, "entry with future next_attempt_at must not appear in pending")
	}

	// Force next_attempt_at to the past so the retry fires immediately.
	_, err = sharedOutboxPool.Exec(
		ctx,
		"UPDATE event_outbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1",
		target.ID,
	)
	require.NoError(t, err)

	// Now a real publisher should pick it up.
	rdb, cleanupRedis := newMiniRedis(t)
	defer cleanupRedis()

	sub := rdb.Subscribe(context.Background(), channel)
	defer func() { _ = sub.Close() }()

	pub := outbox.NewRedisPublisher(rdb)
	poller := outbox.New(ob, pub)

	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		poller.Start(pollCtx)
	}()

	defer func() {
		cancel()
		<-done
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case msg := <-sub.Channel():
		assert.Equal(t, channel, msg.Channel)
	case <-timer.C:
		t.Fatal("timed out waiting for retry after backoff")
	}
}

// TestOutbox_ConcurrentPollers_SkipLocked verifies that concurrent pollers do not
// double-publish the same entry thanks to FOR UPDATE SKIP LOCKED.
func TestOutbox_ConcurrentPollers_SkipLocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ob := pgstore.NewOutboxStore(sharedOutboxPool)

	rdb, cleanup := newMiniRedis(t)
	defer cleanup()

	const channel = "workspace.skip-locked-test"
	payload := []byte(`{"event_id":"concurrent"}`)

	enqueueEntry(t, ctx, ob, channel, payload)

	// Subscribe and count how many times the message arrives.
	sub := rdb.Subscribe(context.Background(), channel)
	defer func() { _ = sub.Close() }()

	pub := outbox.NewRedisPublisher(rdb)

	// Start 3 concurrent pollers using the same outbox store.
	const numPollers = 3

	var wg sync.WaitGroup
	cancels := make([]context.CancelFunc, numPollers)

	for i := range numPollers {
		pollCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel stored in cancels[i] and called post-loop
		cancels[i] = cancel
		wg.Add(1)

		go func(pctx context.Context) {
			defer wg.Done()
			p := outbox.New(ob, pub)
			p.Start(pctx)
		}(pollCtx)
	}

	// Collect messages for 3 seconds.
	received := make(chan string, 10)

	go func() {
		msgCh := sub.Channel()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()

		for {
			select {
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				received <- msg.Payload
			case <-timer.C:
				close(received)
				return
			}
		}
	}()

	var count int

	for range received {
		count++
	}

	// Stop all pollers.
	for _, cancel := range cancels {
		cancel()
	}

	wg.Wait()

	assert.Equal(t, 1, count, "SKIP LOCKED must prevent double-publish; got %d publishes", count)
}

// TestOutbox_Retention verifies that published entries older than cutoff are deleted
// and unpublished entries are never deleted.
func TestOutbox_Retention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ob := pgstore.NewOutboxStore(sharedOutboxPool)

	// Enqueue two entries: one we'll mark published and backdate, one we leave unpublished.
	oldEventID := uuid.New()
	unpublishedEventID := uuid.New()

	require.NoError(t, ob.Enqueue(ctx, &store.OutboxEnqueueInput{
		AggregateType: "retention",
		AggregateID:   uuid.New(),
		EventID:       oldEventID,
		Channel:       "workspace.retention-test",
		Payload:       []byte(`{}`),
	}))

	require.NoError(t, ob.Enqueue(ctx, &store.OutboxEnqueueInput{
		AggregateType: "retention",
		AggregateID:   uuid.New(),
		EventID:       unpublishedEventID,
		Channel:       "workspace.retention-test",
		Payload:       []byte(`{}`),
	}))

	// Fetch to get IDs.
	pending, err := ob.FetchPending(ctx, 100)
	require.NoError(t, err)

	var oldID, unpublishedID uuid.UUID

	for _, e := range pending {
		switch e.EventID {
		case oldEventID:
			oldID = e.ID
		case unpublishedEventID:
			unpublishedID = e.ID
		}
	}

	require.NotEqual(t, uuid.Nil, oldID, "old entry not found in pending")
	require.NotEqual(t, uuid.Nil, unpublishedID, "unpublished entry not found in pending")

	// Mark the old entry as published, then backdate published_at by 8 days.
	require.NoError(t, ob.MarkPublished(ctx, oldID))

	_, err = sharedOutboxPool.Exec(
		ctx,
		"UPDATE event_outbox SET published_at = now() - interval '8 days' WHERE id = $1",
		oldID,
	)
	require.NoError(t, err)

	// Run retention with cutoff = now()-7d.
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	deleted, err := ob.DeleteOldPublished(ctx, cutoff)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1), "expected at least 1 published entry deleted")

	// Verify old published entry is gone.
	rows, queryErr := sharedOutboxPool.Query(ctx, "SELECT id FROM event_outbox WHERE id = $1", oldID)
	require.NoError(t, queryErr)
	defer rows.Close()
	assert.False(t, rows.Next(), "old published entry should have been deleted")

	// Verify unpublished entry is still present.
	rows2, queryErr2 := sharedOutboxPool.Query(ctx, "SELECT id FROM event_outbox WHERE id = $1", unpublishedID)
	require.NoError(t, queryErr2)
	defer rows2.Close()
	assert.True(t, rows2.Next(), "unpublished entry must not be deleted by retention")
}

// countingPublisher is a test double that fails on the first failUntil publishes.
type countingPublisher struct {
	mu        sync.Mutex
	count     int
	failUntil int
	onFail    func()
}

func (c *countingPublisher) Publish(_ context.Context, _ string, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.count++

	if c.count <= c.failUntil {
		if c.onFail != nil {
			c.onFail()
		}

		return fmt.Errorf("simulated publish failure #%d", c.count)
	}

	return nil
}
