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
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
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
type recordingPublisher struct {
	events.NoopPublisher
	completed []*domain.MultipartyContractCompletedEvent
}

func (r *recordingPublisher) PublishMultipartyContractCompleted(_ context.Context, evt *domain.MultipartyContractCompletedEvent) error {
	r.completed = append(r.completed, evt)
	return nil
}

// startMilestoneTestDB spins up a Postgres testcontainer, applies all migrations,
// and returns a populated milestoneTestEnv.
func startMilestoneTestDB(t *testing.T, ctx context.Context) *milestoneTestEnv {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolConfig{})
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	// Apply all embedded migrations.
	var upFiles []string

	err = fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, upFiles, "no *.up.sql files found")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s", file)
	}

	mpContracts := postgres.NewMultipartyContractStore(pool)
	mpParties := postgres.NewMultipartyPartyStore(pool)
	mpSigs := postgres.NewMultipartySignatureStore(pool)
	mpTx := postgres.NewMultipartyTxManager(pool)
	msStore := postgres.NewMilestoneStore(pool)

	pub := &recordingPublisher{}
	mpSvc := service.NewMultipartyContractService(mpContracts, mpParties, mpSigs, mpTx, pub)
	milestoneSvc := service.NewMilestoneService(mpContracts, msStore, mpParties, pub)

	return &milestoneTestEnv{
		mpSvc:          mpSvc,
		milestoneSvc:   milestoneSvc,
		contractStore:  mpContracts,
		milestoneStore: msStore,
		pub:            pub,
	}
}

// setupActiveContract creates a multiparty contract with N parties and activates it.
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
	currency := "TWD"

	// Create contract with a single party (10000 bps) and poster.
	c, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     10000,
		Currency:     &currency,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	return c, posterID
}

// TestMilestone_MigrationsApply verifies migration 000007 tables and columns exist.
func TestMilestone_MigrationsApply(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
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
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolConfig{})
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	var upFiles []string

	err = fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, err)

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply %s", file)
	}

	// multiparty_milestones table must exist.
	var count int
	row := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name='multiparty_milestones' AND table_schema='public'`)
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 1, count, "multiparty_milestones table must exist after migration 000007")

	// poster_user_id column must exist on multi_party_contracts.
	row = pool.QueryRow(ctx,
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
			Currency:   "TWD",
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
			Currency:   "TWD",
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
			Currency:   "TWD",
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
				Currency:   "TWD",
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
			Currency:   "TWD",
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

		// Assert the published event shape.
		require.Len(t, env.pub.completed, 1, "exactly one contract_completed event must be published")

		evt := env.pub.completed[0]
		assert.NotEqual(t, uuid.Nil, evt.EventID)
		assert.Equal(t, 1, evt.Version)
		assert.Equal(t, contract.ID, evt.Data.ContractID)
		assert.Equal(t, contract.TenderID, evt.Data.TenderID)
		assert.Equal(t, m.ID, evt.Data.MilestoneID)
		assert.True(t, decimal.NewFromFloat(7500.50).Equal(evt.Data.Amount))
		assert.Equal(t, "TWD", evt.Data.Currency)
	})

	t.Run("non-owner cannot complete milestone (IDOR)", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)
		contract, posterID := setupActiveContract(t, ctx, env)

		m, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contract.ID,
			CallerID:   posterID,
			Name:       "M1",
			Amount:     decimal.NewFromInt(1000),
			Currency:   "TWD",
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
			Currency:   "TWD",
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

		contractA, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     uuid.New(),
			VendorUserID: uuid.New(),
			ShareBps:     10000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		contractB, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     uuid.New(),
			VendorUserID: uuid.New(),
			ShareBps:     10000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		// Add milestone to contract A.
		mA, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
			ContractID: contractA.ID,
			CallerID:   posterID,
			Name:       "M-A",
			Amount:     decimal.NewFromInt(500),
			Currency:   "TWD",
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

// TestMilestone_Roster_ReturnsActiveParties proves the roster service method returns
// the frozen ACTIVE-party list with correct vendorUserId and shareBps, and returns
// 404 (ErrMultipartyContractNotFound) for a phantom/non-existent contract ID.
func TestMilestone_Roster_ReturnsActiveParties(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone integration test in short mode")
	}

	ctx := context.Background()

	t.Run("returns active parties for existing contract", func(t *testing.T) {
		env := startMilestoneTestDB(t, ctx)

		tenderID := uuid.New()
		vendorA := uuid.New()
		vendorB := uuid.New()
		posterID := uuid.New()

		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorA,
			ShareBps:     6000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: vendorB,
			ShareBps:     4000,
		})
		require.NoError(t, err)

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
			Currency:   "TWD",
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
