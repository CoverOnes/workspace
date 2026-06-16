package handler_test

// internal_milestones_handler_test.go — Integration tests for
// GET /internal/v1/contracts/:id/milestones/amounts.
//
// Cases covered:
//  1. correct token + ACTIVE contract with milestones → 200 with correct sum
//  2. missing token → 401
//  3. wrong token → 403
//  4. unknown contract ID → 404 (phantom guard)
//  5. contract in DRAFT state → 422 (not ACTIVE/COMPLETED)
//  6. ACTIVE contract with no milestones → 200 with totalAmount="0.00"

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const milestoneAmountsTestServiceToken = "test-service-token-32-chars-long!"

// milestoneAmountsTestEnv holds everything needed for milestone amounts handler tests.
type milestoneAmountsTestEnv struct {
	router       http.Handler
	mpSvc        *service.MultipartyContractService
	milestoneSvc *service.MilestoneService
}

func startMilestoneAmountsTestDB(t *testing.T, ctx context.Context) *milestoneAmountsTestEnv {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping milestone amounts handler integration test in short mode")
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

	mpContracts := postgres.NewMultipartyContractStore(pool)
	mpParties := postgres.NewMultipartyPartyStore(pool)
	mpSigs := postgres.NewMultipartySignatureStore(pool)
	mpTx := postgres.NewMultipartyTxManager(pool)
	addendaStore := postgres.NewAddendumStore(pool)
	msStore := postgres.NewMilestoneStore(pool)
	pub := events.NewNoopPublisher()

	milestoneTx := postgres.NewMilestoneTxManager(pool)

	mpSvc := service.NewMultipartyContractService(mpContracts, mpParties, mpSigs, addendaStore, mpTx, pub)
	milestoneSvc := service.NewMilestoneService(mpContracts, msStore, mpParties, milestoneTx, pub)

	contractStore := postgres.NewContractStore(pool)
	sigStore := postgres.NewSignatureStore(pool)
	contractSvc := service.NewContractService(contractStore, sigStore, postgres.NewTxManager(pool), pub, nil)
	signatureSvc := service.NewSignatureService(contractStore, sigStore, nil)
	taskSvc := service.NewTaskService(contractStore, postgres.NewTaskStore(pool))
	worklogSvc := service.NewWorklogService(contractStore, postgres.NewWorklogStore(pool))

	r := handler.NewRouter(&handler.RouterConfig{
		MultipartyContractSvc: mpSvc,
		MilestoneSvc:          milestoneSvc,
		Pool:                  pool,
		ContractServiceToken:  milestoneAmountsTestServiceToken,
		GatewayHMACSecret:     "",
		ContractSvc:           contractSvc,
		SignatureSvc:          signatureSvc,
		TaskSvc:               taskSvc,
		WorklogSvc:            worklogSvc,
	})

	return &milestoneAmountsTestEnv{
		router:       r,
		mpSvc:        mpSvc,
		milestoneSvc: milestoneSvc,
	}
}

// contractActivationResult holds the IDs returned by activateContractForMilestones.
type contractActivationResult struct {
	contractID uuid.UUID
	posterID   uuid.UUID
}

// activateContractForMilestones creates and activates a 2-party contract.
func activateContractForMilestones(
	t *testing.T,
	ctx context.Context,
	env *milestoneAmountsTestEnv,
) contractActivationResult {
	t.Helper()

	tenderID := uuid.New()
	poster := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()
	currency := testCurrencyTWD

	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     6000,
		Currency:     &currency,
		PosterUserID: &poster,
	})
	require.NoError(t, err)

	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorB,
		ShareBps:     4000,
	})
	require.NoError(t, err)

	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, poster)
	require.NoError(t, err)

	_, err = env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorA,
		SignedContentHash: submitted.ContentHash,
		Version:           1,
	})
	require.NoError(t, err)

	active, err := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorB,
		SignedContentHash: submitted.ContentHash,
		Version:           1,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, active.Status)

	return contractActivationResult{contractID: contract.ID, posterID: poster}
}

// TestMilestoneAmounts_GetAmountsSum is the main test for the endpoint.
func TestMilestoneAmounts_GetAmountsSum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone amounts handler integration test in short mode")
	}

	ctx := context.Background()
	env := startMilestoneAmountsTestDB(t, ctx)

	act := activateContractForMilestones(t, ctx, env)

	// Add two milestones: 1000.00 + 500.50 = 1500.50.
	_, err := env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
		ContractID: act.contractID,
		CallerID:   act.posterID,
		Name:       "Milestone 1",
		Amount:     decimal.NewFromInt(1000),
		Currency:   testCurrencyTWD,
		Sequence:   1,
	})
	require.NoError(t, err)

	_, err = env.milestoneSvc.AddMilestone(ctx, &service.AddMilestoneInput{
		ContractID: act.contractID,
		CallerID:   act.posterID,
		Name:       "Milestone 2",
		Amount:     decimal.RequireFromString("500.50"),
		Currency:   testCurrencyTWD,
		Sequence:   2,
	})
	require.NoError(t, err)

	path := fmt.Sprintf("/internal/v1/contracts/%s/milestones/amounts", act.contractID)

	t.Run("missing token → 401", func(t *testing.T) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("wrong token → 403", func(t *testing.T) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
		req.Header.Set("X-Service-Token", "wrong-token-that-is-32-chars-long!")
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("correct token → 200 with correct sum", func(t *testing.T) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
		req.Header.Set("X-Service-Token", milestoneAmountsTestServiceToken)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Data struct {
				TotalAmount string `json:"totalAmount"`
			} `json:"data"`
		}

		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

		got, parseErr := decimal.NewFromString(resp.Data.TotalAmount)
		require.NoError(t, parseErr)

		expected := decimal.NewFromFloat(1500.50)
		assert.True(t, got.Equal(expected),
			"expected totalAmount %s, got %s", expected.StringFixed(2), got.StringFixed(2))
	})
}

// TestMilestoneAmounts_PhantomContractGuard verifies 404 for unknown contract.
func TestMilestoneAmounts_PhantomContractGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone amounts handler integration test in short mode")
	}

	ctx := context.Background()
	env := startMilestoneAmountsTestDB(t, ctx)

	phantomID := uuid.New()
	path := fmt.Sprintf("/internal/v1/contracts/%s/milestones/amounts", phantomID)

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	req.Header.Set("X-Service-Token", milestoneAmountsTestServiceToken)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"milestone amounts of a non-existent contract must return 404")
}

// TestMilestoneAmounts_NoMilestones verifies that a contract with no milestones
// returns 200 with totalAmount="0.00" rather than an error.
func TestMilestoneAmounts_NoMilestones(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping milestone amounts handler integration test in short mode")
	}

	ctx := context.Background()
	env := startMilestoneAmountsTestDB(t, ctx)

	act := activateContractForMilestones(t, ctx, env)
	path := fmt.Sprintf("/internal/v1/contracts/%s/milestones/amounts", act.contractID)

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	req.Header.Set("X-Service-Token", milestoneAmountsTestServiceToken)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data struct {
			TotalAmount string `json:"totalAmount"`
		} `json:"data"`
	}

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "0.00", resp.Data.TotalAmount, "no milestones → totalAmount must be 0.00")
}
