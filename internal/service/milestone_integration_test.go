package service_test

// Phase 3 integration tests for the milestone model on multiparty contracts.
//
// Tests:
//  1. Migrations apply (multiparty_milestones table + poster_user_id column exist).
//  2. Add milestone — only owner can add (non-owner → ErrNotContractOwner / 404).
//  3. List milestones — only owner can list (non-owner → ErrNotContractOwner).
//  4. Complete milestone — sets COMPLETED, emits workspace.contract_completed event.
//  5. Complete already-completed milestone → ErrMilestoneAlreadyDone.
//  6. Complete milestone from wrong contract → ErrMilestoneNotFound (IDOR guard).
//  7. GetPartyRoster: returns [{vendorUserId, shareBps}] for existing contract;
//     returns ErrMultipartyContractNotFound for phantom contract ID (404 guard).
//  8. Sum-of-milestone-amounts: NOT enforced (documented decision — poster adds incrementally).

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// milestoneTestEnv holds all stores and services for milestone integration tests.
type milestoneTestEnv struct {
	mpSvc          *service.MultipartyContractService
	milestoneSvc   *service.MilestoneService
	contractStore  *postgres.MultipartyContractStore
	milestoneStore *postgres.MilestoneStore
	pub            *recordingPublisher
}

// recordingPublisher records published events for assertion.
// The mu guards all access to completed because PublishMultipartyContractCompleted is
// called from a detached goroutine (post-commit best-effort publish) while the test
// goroutine may concurrently call count() — without the mutex this is a data race.
type recordingPublisher struct {
	events.NoopPublisher
	mu        sync.Mutex
	completed []*domain.MultipartyContractCompletedEvent
}

func (r *recordingPublisher) PublishMultipartyContractCompleted(_ context.Context, evt *domain.MultipartyContractCompletedEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed = append(r.completed, evt)
	return nil
}

// count returns the number of recorded events in a race-safe manner.
func (r *recordingPublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.completed)
}

// snapshot returns a copy of all recorded events under the lock.
func (r *recordingPublisher) snapshot() []*domain.MultipartyContractCompletedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MultipartyContractCompletedEvent, len(r.completed))
	copy(out, r.completed)
	return out
}

// startMilestoneTestDB returns a populated milestoneTestEnv backed by the singleton
// sharedServicePool (started once in TestMain).  No new container is started here.
// Each call creates a fresh recordingPublisher and fresh service instances so there
// is no state leakage between tests (data isolation comes from unique UUIDs per test).
func startMilestoneTestDB(t *testing.T, _ context.Context) *milestoneTestEnv {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

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

	return &milestoneTestEnv{
		mpSvc:          mpSvc,
		milestoneSvc:   milestoneSvc,
		contractStore:  mpContracts,
		milestoneStore: msStore,
		pub:            pub,
	}
}

// setupActiveContract creates a multiparty contract, submits it for signatures,
// and has the single vendor sign, producing an ACTIVE contract.
// Returns the contract and the poster user ID.
func setupActiveContract(
	t *testing.T,
	ctx context.Context,
	env *milestoneTestEnv,
) (contract *domain.MultipartyContract, posterID uuid.UUID) {
	t.Helper()

	posterID = uuid.New()
	tenderID := uuid.New()
	vendorA := uuid.New()
	currency := testCurrencyTWD

	// Create contract with a single party (10000 bps) and poster — starts as DRAFT.
	c, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     10000,
		Currency:     &currency,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	// Submit DRAFT → PENDING_SIGNATURES (owner-only: callerUserID = posterID).
	submitted, err := env.mpSvc.SubmitForSignatures(ctx, c.ID, posterID)
	require.NoError(t, err)

	// The sole vendor signs → PENDING_SIGNATURES → ACTIVE (quorum = 1/1 satisfied).
	active, err := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        c.ID,
		SignerUserID:      vendorA,
		SignedContentHash: submitted.ContentHash,
		Version:           submitted.Version,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, active.Status,
		"setupActiveContract: contract must be ACTIVE after the sole vendor signs")

	return active, posterID
}

// TestMilestone_MigrationsApply verifies migration 000007 tables and columns exist.
// Uses the singleton sharedServicePool (migrations already applied by TestMain).
func TestMilestone_MigrationsApply(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	ctx := context.Background()

	// multiparty_milestones table must exist.
	var count int
	row := sharedServicePool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name='multiparty_milestones' AND table_schema='public'`)
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 1, count, "multiparty_milestones table must exist after migration 000007")

	// poster_user_id column must exist on multi_party_contracts.
	row = sharedServicePool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_name='multi_party_contracts' AND column_name='poster_user_id' AND table_schema='public'`)
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 1, count, "poster_user_id column must exist on multi_party_contracts")
}

// TestMilestone_Add_OwnerOnly proves IDOR: only the poster may add milestones.
func TestMilestone_Add_OwnerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctx := context.Background()

	t.Run("owner can add milestone", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		m, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   posterID,
			Name:       "Milestone 1",
			Amount:     decimal.NewFromInt(5000),
			Currency:   testCurrencyTWD,
			Sequence:   1,
		})
		require.NoError(t, err)
		assert.Equal(t, "Milestone 1", m.Name)
		assert.Equal(t, domain.MilestoneStatusPending, m.Status)
		assert.Equal(t, contract.ID, m.MultiContractID)
		assert.True(t, decimal.NewFromInt(5000).Equal(m.Amount))
	})

	t.Run("non-owner gets ErrNotContractOwner (IDOR → 404)", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, _ := setupActiveContract(t, ctx, env)
		nonOwner := uuid.New()

		_, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   nonOwner,
			Name:       "Should Fail",
			Amount:     decimal.NewFromInt(1000),
			Currency:   testCurrencyTWD,
			Sequence:   1,
		})
		require.ErrorIs(t, err, domain.ErrNotContractOwner,
			"non-owner adding milestone must return ErrNotContractOwner")
	})

	t.Run("contract without posterUserId blocks all callers", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		// Create contract WITHOUT a poster (mimics Phase-2 rows).
		tenderID := uuid.New()
		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: uuid.New(),
			ShareBps:     10000,
		})
		require.NoError(t, err)
		assert.Nil(t, contract.PosterUserID)

		_, addErr := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   uuid.New(), // any caller
			Name:       "No poster",
			Amount:     decimal.NewFromInt(100),
			Currency:   testCurrencyTWD,
		})
		require.ErrorIs(t, addErr, domain.ErrNotContractOwner,
			"contract with nil PosterUserID must reject all milestone management")
	})
}

// TestMilestone_List_OwnerOnly proves IDOR: only the poster may list milestones.
func TestMilestone_List_OwnerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctx := context.Background()

	t.Run("owner lists milestones in sequence order", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		// Add 3 milestones in reverse sequence order.
		for _, seq := range []int{3, 1, 2} {
			_, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
				ContractID: contract.ID,
				CallerID:   posterID,
				Name:       fmt.Sprintf("M%d", seq),
				Amount:     decimal.NewFromInt(int64(seq * 1000)),
				Currency:   testCurrencyTWD,
				Sequence:   seq,
			})
			require.NoError(t, err)
		}

		ms, err := env.milestoneSvc.ListMilestones(ctx, contract.ID, posterID)
		require.NoError(t, err)
		require.Len(t, ms, 3)

		// Must be ordered by sequence ASC.
		assert.Equal(t, 1, ms[0].Sequence)
		assert.Equal(t, 2, ms[1].Sequence)
		assert.Equal(t, 3, ms[2].Sequence)
	})

	t.Run("non-owner gets ErrNotContractOwner", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, _ := setupActiveContract(t, ctx, env)

		_, err := env.milestoneSvc.ListMilestones(ctx, contract.ID, uuid.New())
		require.ErrorIs(t, err, domain.ErrNotContractOwner,
			"non-owner listing milestones must return ErrNotContractOwner")
	})

	t.Run("empty list returns empty slice (not nil)", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		ms, err := env.milestoneSvc.ListMilestones(ctx, contract.ID, posterID)
		require.NoError(t, err)
		assert.NotNil(t, ms, "empty milestone list must return [] not nil")
		assert.Empty(t, ms)
	})
}

// TestMilestone_Complete proves milestone completion, event emission, and IDOR guards.
func TestMilestone_Complete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctx := context.Background()

	t.Run("owner completes milestone and event is emitted", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		m, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   posterID,
			Name:       "Deliverable 1",
			Amount:     decimal.NewFromFloat(7500.50),
			Currency:   testCurrencyTWD,
			Sequence:   1,
		})
		require.NoError(t, err)

		before := time.Now()
		completed, err := env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
			ContractID:  contract.ID,
			MilestoneID: m.ID,
			CallerID:    posterID,
		})
		require.NoError(t, err)

		assert.Equal(t, domain.MilestoneStatusCompleted, completed.Status)
		assert.NotNil(t, completed.CompletedAt)
		assert.True(t, completed.CompletedAt.After(before) || completed.CompletedAt.Equal(before),
			"completedAt must be set to now")

		// publishCompleted runs in a best-effort detached goroutine. Poll with a
		// generous timeout so we don't flake on loaded CI instead of sleeping a fixed
		// 20 ms that may not be enough.
		require.Eventually(t, func() bool { return env.pub.count() == 1 },
			2*time.Second, 5*time.Millisecond,
			"exactly one contract_completed event must be published within 2 s")

		// Assert the published event shape — use snapshot() for race-safe access.
		evts := env.pub.snapshot()
		require.Len(t, evts, 1, "exactly one contract_completed event must be published")

		evt := evts[0]
		assert.NotEqual(t, uuid.Nil, evt.EventID)
		assert.Equal(t, 1, evt.Version)
		assert.Equal(t, contract.ID, evt.Data.ContractID)
		assert.Equal(t, contract.TenderID, evt.Data.TenderID)
		assert.Equal(t, m.ID, evt.Data.MilestoneID)
		assert.True(t, decimal.NewFromFloat(7500.50).Equal(evt.Data.Amount))
		assert.Equal(t, testCurrencyTWD, evt.Data.Currency)
	})

	t.Run("non-owner cannot complete milestone (IDOR)", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		m, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   posterID,
			Name:       "M1",
			Amount:     decimal.NewFromInt(1000),
			Currency:   testCurrencyTWD,
		})
		require.NoError(t, err)

		_, err = env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
			ContractID:  contract.ID,
			MilestoneID: m.ID,
			CallerID:    uuid.New(), // not the poster
		})
		require.ErrorIs(t, err, domain.ErrNotContractOwner,
			"non-owner completing milestone must return ErrNotContractOwner")
	})

	t.Run("completing already-completed milestone returns ErrMilestoneAlreadyDone", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		m, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   posterID,
			Name:       "M1",
			Amount:     decimal.NewFromInt(1000),
			Currency:   testCurrencyTWD,
		})
		require.NoError(t, err)

		// First completion — must succeed.
		_, err = env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
			ContractID:  contract.ID,
			MilestoneID: m.ID,
			CallerID:    posterID,
		})
		require.NoError(t, err)

		// Second completion — must fail.
		_, err = env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
			ContractID:  contract.ID,
			MilestoneID: m.ID,
			CallerID:    posterID,
		})
		require.ErrorIs(t, err, domain.ErrMilestoneAlreadyDone,
			"double-completing a milestone must return ErrMilestoneAlreadyDone")
	})

	t.Run("completing milestone from wrong contract returns ErrMilestoneNotFound (IDOR)", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)

		// Two separate contracts with the same poster.
		posterID := uuid.New()
		vendorForA := uuid.New()
		vendorForB := uuid.New()

		contractA, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     uuid.New(),
			VendorUserID: vendorForA,
			ShareBps:     10000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		contractB, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     uuid.New(),
			VendorUserID: vendorForB,
			ShareBps:     10000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		// Both contracts must be ACTIVE before milestones can be added.
		activateSinglePartyContract(t, ctx, env.mpSvc, contractA.ID, vendorForA, posterID)
		activateSinglePartyContract(t, ctx, env.mpSvc, contractB.ID, vendorForB, posterID)

		// Add milestone to contract A.
		mA, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contractA.ID,
			CallerID:   posterID,
			Name:       "M-A",
			Amount:     decimal.NewFromInt(500),
			Currency:   testCurrencyTWD,
		})
		require.NoError(t, err)

		// Try to complete milestone A via contract B (cross-contract IDOR attempt).
		_, err = env.milestoneSvc.CompleteMilestone(ctx, service.CompleteMilestoneInput{
			ContractID:  contractB.ID, // wrong contract
			MilestoneID: mA.ID,
			CallerID:    posterID,
		})
		require.ErrorIs(t, err, domain.ErrMilestoneNotFound,
			"completing a milestone via a different contract must return ErrMilestoneNotFound")
	})
}

// activateSinglePartyContract submits a single-party (10000 bps) contract for
// signatures and has the sole vendor sign, producing an ACTIVE contract.
// vendor must be the VendorUserID used in CreateOrAddParty.
// posterID is the contract owner required by the SubmitForSignatures owner-only gate.
func activateSinglePartyContract(
	t *testing.T,
	ctx context.Context,
	svc *service.MultipartyContractService,
	contractID uuid.UUID,
	vendor uuid.UUID,
	posterID uuid.UUID,
) {
	t.Helper()

	submitted, err := svc.SubmitForSignatures(ctx, contractID, posterID)
	require.NoError(t, err)

	active, err := svc.Sign(ctx, service.SignInput{
		ContractID:        contractID,
		SignerUserID:      vendor,
		SignedContentHash: submitted.ContentHash,
		Version:           submitted.Version,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, active.Status,
		"contract must be ACTIVE after the sole vendor signs")
}

// activateContract submits and signs a contract through to ACTIVE status.
// Both vendorA (6000 bps) and vendorB (4000 bps) sign; returns the final ACTIVE contract.
// Requires that the contract already has vendorA and vendorB as parties summing to 10000 bps.
// posterID is the contract owner required by the SubmitForSignatures owner-only gate.
func activateContract(
	t *testing.T,
	ctx context.Context,
	svc *service.MultipartyContractService,
	contractID uuid.UUID,
	vendorA, vendorB uuid.UUID,
	posterID uuid.UUID,
) {
	t.Helper()

	submitted, err := svc.SubmitForSignatures(ctx, contractID, posterID)
	require.NoError(t, err)

	v1Hash := submitted.ContentHash

	_, err = svc.Sign(ctx, service.SignInput{
		ContractID:        contractID,
		SignerUserID:      vendorA,
		SignedContentHash: v1Hash,
		Version:           1,
	})
	require.NoError(t, err)

	active, err := svc.Sign(ctx, service.SignInput{
		ContractID:        contractID,
		SignerUserID:      vendorB,
		SignedContentHash: v1Hash,
		Version:           1,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, active.Status, "contract must be ACTIVE after all parties sign")
}

// TestMilestone_Roster_ReturnsActiveParties proves the roster service method returns
// the frozen ACTIVE-party list with correct vendorUserId and shareBps, returns
// 404 (ErrMultipartyContractNotFound) for a phantom/non-existent contract ID,
// and rejects transient states where shares may be mid-reallocation (M2 guard).
func TestMilestone_Roster_ReturnsActiveParties(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctx := context.Background()

	t.Run("returns active parties for ACTIVE contract", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)

		tenderID := uuid.New()
		vendorA := uuid.New()
		vendorB := uuid.New()
		posterID := uuid.New()
		currency := testCurrencyTWD

		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorA,
			ShareBps:     6000,
			Currency:     &currency,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorB,
			ShareBps:     4000,
		})
		require.NoError(t, err)

		activateContract(t, ctx, env.mpSvc, contract.ID, vendorA, vendorB, posterID)

		parties, err := env.milestoneSvc.GetPartyRoster(ctx, contract.ID)
		require.NoError(t, err)
		require.Len(t, parties, 2)

		shareMap := make(map[uuid.UUID]int)
		for _, p := range parties {
			shareMap[p.VendorUserID] = p.ShareBps
		}

		assert.Equal(t, 6000, shareMap[vendorA], "vendorA must have 6000 bps")
		assert.Equal(t, 4000, shareMap[vendorB], "vendorB must have 4000 bps")
	})

	t.Run("non-existent contract returns ErrMultipartyContractNotFound", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)

		_, err := env.milestoneSvc.GetPartyRoster(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrMultipartyContractNotFound,
			"phantom contract must return ErrMultipartyContractNotFound (mapped to 404)")
	})

	t.Run("ADDENDUM_PENDING contract returns ErrInvalidTransition (M2 status guard)", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)

		tenderID := uuid.New()
		vendorA := uuid.New()
		vendorB := uuid.New()
		vendorC := uuid.New()
		posterID := uuid.New()
		currency := testCurrencyTWD

		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorA,
			ShareBps:     6000,
			Currency:     &currency,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorB,
			ShareBps:     4000,
		})
		require.NoError(t, err)

		activateContract(t, ctx, env.mpSvc, contract.ID, vendorA, vendorB, posterID)

		// Add a third party via addendum → ADDENDUM_PENDING with vendorC at 0 bps.
		_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorC,
			ShareBps:     0,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		// GetPartyRoster must reject ADDENDUM_PENDING (shares are mid-reallocation).
		_, rosterErr := env.milestoneSvc.GetPartyRoster(ctx, contract.ID)
		require.ErrorIs(t, rosterErr, domain.ErrInvalidTransition,
			"roster on ADDENDUM_PENDING contract must return ErrInvalidTransition")
	})
}

// TestMilestone_SumInvariant_NotEnforced documents that milestone amounts are NOT
// required to sum to any contract total. The poster may add milestones incrementally.
func TestMilestone_SumInvariant_NotEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctx := context.Background()
	env := startMilestoneTestDB(t, ctx)

	contract, posterID := setupActiveContract(t, ctx, env)

	// Add milestones with arbitrary amounts (no sum constraint enforced here).
	amounts := []int64{1000, 2000, 3000}
	for i, amt := range amounts {
		_, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   posterID,
			Name:       fmt.Sprintf("M%d", i+1),
			Amount:     decimal.NewFromInt(amt),
			Currency:   testCurrencyTWD,
			Sequence:   i + 1,
		})
		require.NoError(t, err, "adding milestone with amount %d must succeed (no sum constraint)", amt)
	}

	ms, err := env.milestoneSvc.ListMilestones(ctx, contract.ID, posterID)
	require.NoError(t, err)
	assert.Len(t, ms, 3, "all 3 milestones must be stored without sum-constraint rejection")

	// Compute sum as a sanity check for future payment consumption.
	var total decimal.Decimal
	for _, m := range ms {
		total = total.Add(m.Amount)
	}

	assert.True(t, decimal.NewFromInt(6000).Equal(total),
		"sum of all milestones is %s (payment enforces its own check, not workspace)", total.String())
}
