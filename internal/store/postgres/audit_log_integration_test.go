package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKeyStatus is the payload map key used in audit log test fixtures.
const testKeyStatus = "status"

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
	s := postgres.NewAuditLogStore(sharedPool)
	truncateAuditTables(t, ctx)

	contractID := uuid.New()
	actorID := uuid.New()

	t.Run("append three events and verify hash chain is intact", func(t *testing.T) {
		truncateAuditTables(t, ctx)

		events := []struct {
			eventType string
			payload   map[string]any
		}{
			{"CONTRACT_CREATED", map[string]any{testKeyStatus: "DRAFT"}},
			{"CONTRACT_SUBMITTED", map[string]any{testKeyStatus: "PENDING_SIGNATURE"}},
			{"CONTRACT_ACTIVATED", map[string]any{testKeyStatus: "ACTIVE"}},
		}

		for _, ev := range events {
			_, err := s.Append(ctx, &store.AuditAppendInput{
				ContractID: contractID,
				EventType:  ev.eventType,
				ActorID:    actorID,
				Payload:    ev.payload,
			})
			require.NoError(t, err)
		}

		entries, err := s.ListByContract(ctx, contractID)
		require.NoError(t, err)
		require.Len(t, entries, 3)

		// Verify the chain is intact end-to-end.
		intact, verifyErr := domain.VerifyAuditChain(entries)
		require.NoError(t, verifyErr)
		assert.True(t, intact, "appended chain of 3 events must verify as intact")

		// Verify ordering by seq (oldest first).
		assert.Equal(t, "CONTRACT_CREATED", entries[0].EventType)
		assert.Equal(t, "CONTRACT_SUBMITTED", entries[1].EventType)
		assert.Equal(t, "CONTRACT_ACTIVATED", entries[2].EventType)
		assert.Less(t, entries[0].Seq, entries[1].Seq)
		assert.Less(t, entries[1].Seq, entries[2].Seq)

		// Verify prev_hash linkage.
		assert.Empty(t, entries[0].PrevHash, "genesis entry must have empty prev_hash")
		assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
		assert.Equal(t, entries[1].Hash, entries[2].PrevHash)
	})

	t.Run("list returns empty slice for unknown contract", func(t *testing.T) {
		entries, err := s.ListByContract(ctx, uuid.New())
		require.NoError(t, err)
		assert.Empty(t, entries, "unknown contract must return empty slice, not error")
	})

	t.Run("concurrent appends serialize via advisory lock — chain remains intact", func(t *testing.T) {
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

				// Each goroutine calls Append directly; the store acquires the advisory
				// lock, reads the tail, computes the hash, and inserts — all in one tx.
				// Without the lock the five goroutines would race on the tail read and
				// produce a forked chain.
				_, appendErr := s.Append(ctx, &store.AuditAppendInput{
					ContractID: cID,
					EventType:  "CONCURRENT_EVENT",
					ActorID:    aID,
					Payload:    map[string]any{"worker": idx},
				})
				errs[idx] = appendErr
			}(i)
		}

		wg.Wait()

		// All appends must succeed with no errors.
		for i, err := range errs {
			require.NoError(t, err, "goroutine %d must append without error", i)
		}

		// All n entries must be present.
		entries, err := s.ListByContract(ctx, cID)
		require.NoError(t, err)
		require.Len(t, entries, n, "all %d concurrent appends must be persisted", n)

		// The chain must be intact — no forks despite concurrent appends.
		intact, verifyErr := domain.VerifyAuditChain(entries)
		require.NoError(t, verifyErr)
		assert.True(t, intact, "chain must remain intact after %d concurrent appends via advisory lock", n)
	})
}
