package service_test

// Regression tests for the two Critical findings from the 2026-06-06 five-army audit:
//
//   [Critical] AddMilestone must reject non-ACTIVE contracts (DRAFT, CANCELED, etc.)
//   [Critical] CompleteMilestone must reject non-ACTIVE contracts (CANCELED, etc.)
//
// These tests use a real Postgres testcontainer (backend-security-design §6.5).
// They prove that the ErrInvalidTransition guard in AddMilestone and CompleteMilestone
// fires for every non-ACTIVE state and is absent for the ACTIVE state.

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addMilestoneInput returns a minimal valid AddMilestoneInput for a given contract.
func addMilestoneInput(contractID, posterID uuid.UUID) *service.AddMilestoneInput {
	return &service.AddMilestoneInput{
		ContractID: contractID,
		CallerID:   posterID,
		Name:       "Test Milestone",
		Amount:     decimal.NewFromInt(1000),
		Currency:   "TWD",
		Sequence:   1,
	}
}

// TestAddMilestone_RequiresActiveContract is a table-driven integration test proving
// that AddMilestone rejects every non-ACTIVE contract status with ErrInvalidTransition
// and succeeds only for ACTIVE contracts.
func TestAddMilestone_RequiresActiveContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone status guard integration test in short mode")
	}

	ctx := context.Background()

	nonActiveStatuses := []struct {
		name   string
		status domain.MultipartyContractStatus
	}{
		{"DRAFT", domain.MultipartyContractStatusDraft},
		{"PENDING_SIGNATURES", domain.MultipartyContractStatusPendingSignatures},
		{"ADDENDUM_PENDING", domain.MultipartyContractStatusAddendumPending},
		// MultipartyContractStatusCancelled is the Go constant matching the DB CHECK
		// constraint; its underlying SQL value uses the locale spelling from migrations.
		{"CANCELED_SQL", domain.MultipartyContractStatusCancelled},
		{"COMPLETED", domain.MultipartyContractStatusCompleted},
	}

	for _, tc := range nonActiveStatuses {
		t.Run("rejects_"+string(tc.status), func(t *testing.T) {
			env := startMilestoneTestDB(t, ctx)

			// Create a contract in ACTIVE state first (easiest to manipulate to target status).
			contract, posterID := setupActiveContract(t, ctx, env)

			// Force the contract into the target non-ACTIVE status via the store layer.
			// This simulates the state the audit describes (e.g. a contract that went
			// ACTIVE→CANCELED retains its rows but must reject new milestones).
			contract.Status = tc.status
			require.NoError(t, env.contractStore.Update(ctx, contract),
				"force-set contract status to %s for test", tc.status)

			_, err := env.milestoneSvc.AddMilestone(ctx, addMilestoneInput(contract.ID, posterID))
			require.Error(t, err, "AddMilestone on %s contract must return an error", tc.status)
			assert.ErrorIs(t, err, domain.ErrInvalidTransition,
				"AddMilestone on %s contract must return ErrInvalidTransition", tc.status)
		})
	}

	t.Run("allows_ACTIVE", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		// setupActiveContract already produces an ACTIVE contract — no status override.
		m, err := env.milestoneSvc.AddMilestone(ctx, addMilestoneInput(contract.ID, posterID))
		require.NoError(t, err, "AddMilestone on ACTIVE contract must succeed")
		assert.Equal(t, domain.MilestoneStatusPending, m.Status)
		assert.Equal(t, contract.ID, m.MultiContractID)
	})
}

// TestCompleteMilestone_RequiresActiveContract proves that CompleteMilestone rejects
// non-ACTIVE contracts with ErrInvalidTransition and does not publish a disbursement
// event. This is the core money-path regression: a contract in terminal state (CANCELED
// or COMPLETED) with a PENDING milestone must not trigger a workspace.contract_completed event.
func TestCompleteMilestone_RequiresActiveContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone status guard integration test in short mode")
	}

	ctx := context.Background()

	// Error-path cases: all non-ACTIVE statuses.
	nonActiveStatuses := []struct {
		name   string
		status domain.MultipartyContractStatus
	}{
		// MultipartyContractStatusCancelled is the Go constant matching the DB CHECK
		// constraint; its underlying SQL value uses the locale spelling from migrations.
		{"CANCELED_SQL", domain.MultipartyContractStatusCancelled},
		{"DRAFT", domain.MultipartyContractStatusDraft},
		{"PENDING_SIGNATURES", domain.MultipartyContractStatusPendingSignatures},
		{"ADDENDUM_PENDING", domain.MultipartyContractStatusAddendumPending},
		{"COMPLETED", domain.MultipartyContractStatusCompleted},
	}

	for _, tc := range nonActiveStatuses {
		t.Run("rejects_"+string(tc.status)+"_no_event_emitted", func(t *testing.T) {
			env := startMilestoneTestDB(t, ctx)

			// Create an ACTIVE contract + add a milestone while it is still ACTIVE.
			contract, posterID := setupActiveContract(t, ctx, env)

			m, err := env.milestoneSvc.AddMilestone(ctx, addMilestoneInput(contract.ID, posterID))
			require.NoError(t, err, "setup: add milestone while ACTIVE")

			// Now force-transition the contract to a non-ACTIVE status.
			// This is the real-world scenario: a contract was ACTIVE, a milestone was
			// added, then the contract was canceled before the milestone was completed.
			contract.Status = tc.status
			require.NoError(t, env.contractStore.Update(ctx, contract),
				"force-set contract status to %s for test", tc.status)

			// Capture event count before the attempt.
			eventsBefore := len(env.pub.completed)

			_, err = env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
				ContractID:  contract.ID,
				MilestoneID: m.ID,
				CallerID:    posterID,
			})
			require.Error(t, err, "CompleteMilestone on %s contract must return an error", tc.status)
			assert.ErrorIs(t, err, domain.ErrInvalidTransition,
				"CompleteMilestone on %s contract must return ErrInvalidTransition", tc.status)

			// Critical: no workspace.contract_completed event must have been emitted.
			// This is the disbursement event consumed by the payment service.
			assert.Equal(t, eventsBefore, len(env.pub.completed),
				"CompleteMilestone on %s contract must NOT emit a disbursement event", tc.status)
		})
	}

	t.Run("allows_ACTIVE_and_emits_event", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		m, err := env.milestoneSvc.AddMilestone(ctx, addMilestoneInput(contract.ID, posterID))
		require.NoError(t, err)

		completed, err := env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
			ContractID:  contract.ID,
			MilestoneID: m.ID,
			CallerID:    posterID,
		})
		require.NoError(t, err, "CompleteMilestone on ACTIVE contract must succeed")
		assert.Equal(t, domain.MilestoneStatusCompleted, completed.Status)

		// publishCompleted runs in a best-effort detached goroutine; give it a brief
		// window to complete the in-memory append before asserting event count.
		time.Sleep(20 * time.Millisecond)

		assert.Len(t, env.pub.completed, 1,
			"CompleteMilestone on ACTIVE contract must emit exactly one disbursement event")
	})
}
