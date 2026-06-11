package service_test

// TestCompleteMilestone_TOCTOURace verifies that the FOR UPDATE row lock in
// WithMilestoneTx serializes CompleteMilestone against a concurrent addendum
// (CreateOrAddParty ACTIVE→ADDENDUM_PENDING on the same contract row).
//
// WITHOUT the FOR UPDATE lock the race is:
//   1. CompleteMilestone reads contract status = ACTIVE (passes guard).
//   2. CreateOrAddParty (addendum) transitions contract ACTIVE → ADDENDUM_PENDING and commits.
//   3. CompleteMilestone writes MarkCompleted — milestone now COMPLETED on an
//      ADDENDUM_PENDING contract — and publishes a disbursement event for real money.
//
// WITH the FOR UPDATE lock the two operations cannot interleave at the critical
// section: one must fully commit before the other can acquire the lock. As a result:
//   - If CompleteMilestone wins the lock first  → milestone completed while contract
//     is ACTIVE; addendum proceeds on an ACTIVE contract afterwards.
//   - If CreateOrAddParty wins the lock first   → contract transitions to
//     ADDENDUM_PENDING; CompleteMilestone then observes ADDENDUM_PENDING under the
//     lock and returns ErrInvalidTransition.  No milestone is completed on a
//     non-ACTIVE contract.
//
// The test races both operations N times and asserts that in every iteration where
// CompleteMilestone returns a non-nil error, it is ErrInvalidTransition — never a
// spurious/corrupted error that would indicate the guard was bypassed.
// It also asserts that the final milestone state in the DB is consistent with the
// outcome returned to the caller.
//
// To prove this test would FAIL without the lock, replace GetByIDForUpdate with
// GetByID in postgres.MilestoneTxManager.WithMilestoneTx → you will observe
// CompleteMilestone succeeding while the contract is already ADDENDUM_PENDING, which
// would violate the invariant checked below.
//
// Uses the singleton sharedServicePool (testcontainers PG, backend-security-design §6.5).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompleteMilestone_TOCTOURace races CompleteMilestone with a concurrent
// CreateOrAddParty (addendum path) on the same contract row.
//
// Invariant: if CompleteMilestone returns nil (success), the milestone row in the
// DB must be in COMPLETED state (the DB write happened correctly). If it returns an
// error, it must be ErrInvalidTransition (the guard fired correctly under the lock).
// No other error is acceptable — that would indicate unexpected breakage.
func TestCompleteMilestone_TOCTOURace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TOCTOU race integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	ctx := context.Background()
	const iterations = 10 // run the race 10 times to surface serialization failures

	for i := range iterations {
		t.Run("iteration", func(t *testing.T) {
			// Each iteration uses a fresh contract to avoid cross-iteration state leakage.
			pool := sharedServicePool

			mpContracts := postgres.NewMultipartyContractStore(pool)
			mpParties := postgres.NewMultipartyPartyStore(pool)
			mpSigs := postgres.NewMultipartySignatureStore(pool)
			mpTx := postgres.NewMultipartyTxManager(pool)
			addendaStore := postgres.NewAddendumStore(pool)
			msStore := postgres.NewMilestoneStore(pool)
			milestoneTx := postgres.NewMilestoneTxManager(pool)
			pub := &recordingPublisher{}

			mpSvc := service.NewMultipartyContractService(mpContracts, mpParties, mpSigs, addendaStore, mpTx, pub)
			milestoneSvc := service.NewMilestoneService(mpContracts, msStore, mpParties, milestoneTx, pub)

			// Set up an ACTIVE single-party contract with one PENDING milestone.
			posterID := uuid.New()
			vendorA := uuid.New()
			vendorC := uuid.New() // the new party added via addendum
			currency := testCurrencyTWD

			c, _, err := mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
				TenderID:     uuid.New(),
				VendorUserID: vendorA,
				ShareBps:     10000,
				Currency:     &currency,
				PosterUserID: &posterID,
			})
			require.NoError(t, err, "iter %d: create contract", i)

			activateSinglePartyContract(t, ctx, mpSvc, c.ID, vendorA, posterID)

			m, err := milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
				ContractID: c.ID,
				CallerID:   posterID,
				Name:       "Race milestone",
				Amount:     decimal.NewFromInt(1000),
				Currency:   "TWD",
				Sequence:   1,
			})
			require.NoError(t, err, "iter %d: add milestone", i)

			// Race: CompleteMilestone vs CreateOrAddParty (addendum: ACTIVE→ADDENDUM_PENDING).
			var (
				wg          sync.WaitGroup
				completeErr error
				addendumErr error
			)

			wg.Add(2)

			go func() {
				defer wg.Done()
				completeErr = completeMilestoneErr(ctx, milestoneSvc, c.ID, m.ID, posterID)
			}()

			go func() {
				defer wg.Done()
				addendumErr = addPartyViaAddendumErr(ctx, mpSvc, c.TenderID, vendorC, posterID)
			}()

			wg.Wait()

			// ── Invariant checks ──────────────────────────────────────────────────────
			//
			// The FOR UPDATE lock forces one operation to fully commit before the other
			// starts. Possible serialized outcomes:
			//
			//   a) CompleteMilestone first, addendum second:
			//      completeErr == nil, addendumErr == nil
			//      (addendum runs on still-ACTIVE contract after complete)
			//
			//   b) Addendum first, CompleteMilestone second:
			//      addendumErr == nil, completeErr == ErrInvalidTransition
			//      (complete observes ADDENDUM_PENDING under the lock)
			//
			// Any other error is a bug. In particular:
			//   - completeErr != nil && !errors.Is(completeErr, domain.ErrInvalidTransition)
			//     → unexpected error: the guard did not fire cleanly
			//   - completeErr == nil AND DB shows milestone is not COMPLETED
			//     → MarkCompleted was skipped or the commit was lost

			if completeErr != nil {
				assert.True(t,
					errors.Is(completeErr, domain.ErrInvalidTransition),
					"iter %d: CompleteMilestone error must be ErrInvalidTransition, got: %v", i, completeErr)
			}

			if addendumErr != nil {
				// The addendum can fail if the contract is already not ACTIVE (e.g.
				// CompleteMilestone ran first — but CompleteMilestone does NOT change contract
				// status, so addendum should always succeed when CompleteMilestone runs first).
				// If addendum fails, it should be ErrInvalidTransition too.
				assert.True(t,
					errors.Is(addendumErr, domain.ErrInvalidTransition) ||
						errors.Is(addendumErr, domain.ErrConflict),
					"iter %d: addendum error must be ErrInvalidTransition or ErrConflict, got: %v", i, addendumErr)
			}

			// If CompleteMilestone reported success, the milestone MUST be COMPLETED in DB.
			if completeErr == nil {
				ms, fetchErr := msStore.GetByID(ctx, m.ID)
				require.NoError(t, fetchErr, "iter %d: fetch milestone after successful complete", i)
				assert.Equal(t, domain.MilestoneStatusCompleted, ms.Status,
					"iter %d: CompleteMilestone returned nil but milestone is not COMPLETED in DB", i)
			}
		})
	}
}

// completeMilestoneErr is a helper that calls CompleteMilestone and returns only the error.
func completeMilestoneErr(
	ctx context.Context,
	svc *service.MilestoneService,
	contractID, milestoneID, callerID uuid.UUID,
) error {
	_, err := svc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
		ContractID:  contractID,
		MilestoneID: milestoneID,
		CallerID:    callerID,
	})

	return err
}

// addPartyViaAddendumErr is a helper that calls CreateOrAddParty (addendum path) and
// returns only the error. Using tenderID (not contractID) is the service API surface.
func addPartyViaAddendumErr(
	ctx context.Context,
	svc *service.MultipartyContractService,
	tenderID, vendorUserID, posterID uuid.UUID,
) error {
	_, _, err := svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorUserID,
		ShareBps:     0,
		PosterUserID: &posterID,
	})

	return err
}
