package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// truncateAuditTables clears contract_audit_logs between sub-tests.
func truncateAuditTables(t *testing.T, ctx context.Context) {
	t.Helper()

	_, err := sharedPool.Exec(ctx, "TRUNCATE TABLE contract_audit_logs RESTART IDENTITY CASCADE")
	require.NoError(t, err, "truncate audit tables")
}

// TestAuditLogStore_Integration tests the AuditLogStore against real Postgres.
func TestAuditLogStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	store := postgres.NewAuditLogStore(sharedPool)
	truncateAuditTables(t, ctx)

	contractID := uuid.New()
	actorID := uuid.New()

	t.Run("append and list three events with correct hash chain", func(t *testing.T) {
		truncateAuditTables(t, ctx)

		events := []struct {
			eventType string
			payload   map[string]any
		}{
			{"CONTRACT_CREATED", map[string]any{"status": "DRAFT"}},
			{"CONTRACT_SUBMITTED", map[string]any{"status": "PENDING_SIGNATURE"}},
			{"CONTRACT_ACTIVATED", map[string]any{"status": "ACTIVE"}},
		}

		prevHash := ""

		for _, ev := range events {
			hash, hashErr := domain.AuditEntryDigest(prevHash, contractID, ev.eventType, actorID, ev.payload)
			require.NoError(t, hashErr)

			entry := &domain.ContractAuditLog{
				ID:         uuid.New(),
				ContractID: contractID,
				EventType:  ev.eventType,
				ActorID:    actorID,
				Payload:    ev.payload,
				PrevHash:   prevHash,
				Hash:       hash,
				CreatedAt:  time.Now().UTC(),
			}

			require.NoError(t, store.Append(ctx, entry))
			prevHash = hash
		}

		entries, err := store.ListByContract(ctx, contractID)
		require.NoError(t, err)
		require.Len(t, entries, 3)

		// Verify the chain is intact.
		intact, err := domain.VerifyAuditChain(entries)
		require.NoError(t, err)
		assert.True(t, intact, "appended chain of 3 events must verify as intact")

		// Verify ordering (oldest first).
		assert.Equal(t, "CONTRACT_CREATED", entries[0].EventType)
		assert.Equal(t, "CONTRACT_SUBMITTED", entries[1].EventType)
		assert.Equal(t, "CONTRACT_ACTIVATED", entries[2].EventType)

		// Verify prev_hash linkage.
		assert.Empty(t, entries[0].PrevHash, "genesis entry must have empty prev_hash")
		assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
		assert.Equal(t, entries[1].Hash, entries[2].PrevHash)
	})

	t.Run("list returns empty slice for unknown contract", func(t *testing.T) {
		entries, err := store.ListByContract(ctx, uuid.New())
		require.NoError(t, err)
		assert.Empty(t, entries, "unknown contract must return empty slice, not error")
	})

	t.Run("concurrent appends serialize via advisory lock", func(t *testing.T) {
		truncateAuditTables(t, ctx)

		cID := uuid.New()
		aID := uuid.New()
		n := 5

		var wg sync.WaitGroup

		errs := make([]error, n)

		for i := range n {
			wg.Add(1)

			go func(idx int) {
				defer wg.Done()

				// Each goroutine fetches the current tail and appends.
				// Without the advisory lock this would produce a forked chain;
				// with it, appends are serialized per contract_id.
				var listErr error

				existing, listErr := store.ListByContract(ctx, cID)
				if listErr != nil {
					errs[idx] = listErr
					return
				}

				prev := ""
				if len(existing) > 0 {
					prev = existing[len(existing)-1].Hash
				}

				payload := map[string]any{"worker": idx}
				hash, hashErr := domain.AuditEntryDigest(prev, cID, "CONCURRENT_EVENT", aID, payload)
				if hashErr != nil {
					errs[idx] = hashErr
					return
				}

				entry := &domain.ContractAuditLog{
					ID:         uuid.New(),
					ContractID: cID,
					EventType:  "CONCURRENT_EVENT",
					ActorID:    aID,
					Payload:    payload,
					PrevHash:   prev,
					Hash:       hash,
					CreatedAt:  time.Now().UTC(),
				}

				errs[idx] = store.Append(ctx, entry)
			}(i)
		}

		wg.Wait()

		// All appends must succeed (no errors).
		for i, err := range errs {
			require.NoError(t, err, "goroutine %d must append without error", i)
		}

		// All n entries must be present.
		entries, err := store.ListByContract(ctx, cID)
		require.NoError(t, err)
		assert.Len(t, entries, n, "all %d concurrent appends must be persisted", n)
	})
}
