package handler_test

// Roster handler integration tests — verify the S2S GET /internal/v1/contracts/:id/parties
// endpoint is:
//  1. Reachable with a correct X-Service-Token.
//  2. Rejects requests without X-Service-Token (401).
//  3. Rejects requests with an incorrect X-Service-Token (403).
//  4. NOT reachable on the public /v1 group (i.e., the path does not exist there).
//  5. Returns the correct [{vendorUserId, shareBps}] roster.
//  6. Returns 404 for a valid token + non-existent contract ID (phantom guard).

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

	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const rosterTestServiceToken = "test-service-token-32-chars-long!"

// rosterTestEnv holds everything needed for roster handler tests.
type rosterTestEnv struct {
	router http.Handler
	mpSvc  *service.MultipartyContractService
}

func startRosterTestDB(t *testing.T, ctx context.Context) *rosterTestEnv {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping roster handler integration test in short mode")
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
	msStore := postgres.NewMilestoneStore(pool)
	pub := events.NewNoopPublisher()

	mpSvc := service.NewMultipartyContractService(mpContracts, mpParties, mpSigs, mpTx, pub)
	milestoneSvc := service.NewMilestoneService(mpContracts, msStore, mpParties, pub)

	contractStore := postgres.NewContractStore(pool)
	sigStore := postgres.NewSignatureStore(pool)
	contractSvc := service.NewContractService(contractStore, sigStore, postgres.NewTxManager(pool), pub)
	signatureSvc := service.NewSignatureService(contractStore, sigStore)
	taskSvc := service.NewTaskService(contractStore, postgres.NewTaskStore(pool))
	worklogSvc := service.NewWorklogService(contractStore, postgres.NewWorklogStore(pool))

	r := handler.NewRouter(&handler.RouterConfig{
		MultipartyContractSvc: mpSvc,
		MilestoneSvc:          milestoneSvc,
		Pool:                  pool,
		ContractServiceToken:  rosterTestServiceToken,
		GatewayHMACSecret:     "", // dev mode — no gateway HMAC
		ContractSvc:           contractSvc,
		SignatureSvc:          signatureSvc,
		TaskSvc:               taskSvc,
		WorklogSvc:            worklogSvc,
	})

	return &rosterTestEnv{
		router: r,
		mpSvc:  mpSvc,
	}
}

// TestRoster_GetParties_RequiresServiceToken verifies S2S gate enforcement.
func TestRoster_GetParties_RequiresServiceToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping roster handler integration test in short mode")
	}

	ctx := context.Background()
	env := startRosterTestDB(t, ctx)

	// Create a contract with two parties.
	tenderID := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()
	posterID := uuid.New()

	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     7000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorB,
		ShareBps:     3000,
	})
	require.NoError(t, err)

	path := fmt.Sprintf("/internal/v1/contracts/%s/parties", contract.ID)

	t.Run("no token → 401", func(t *testing.T) {
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

	t.Run("correct token → 200 with roster", func(t *testing.T) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
		req.Header.Set("X-Service-Token", rosterTestServiceToken)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Data []struct {
				VendorUserID uuid.UUID `json:"vendorUserId"`
				ShareBps     int       `json:"shareBps"`
			} `json:"data"`
		}

		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		require.Len(t, resp.Data, 2)

		shareMap := make(map[uuid.UUID]int)
		for _, p := range resp.Data {
			shareMap[p.VendorUserID] = p.ShareBps
		}

		assert.Equal(t, 7000, shareMap[vendorA])
		assert.Equal(t, 3000, shareMap[vendorB])
	})

	t.Run("roster NOT reachable on public /v1 group", func(t *testing.T) {
		// The public group is at /v1, not /internal/v1; this path must 404.
		publicPath := fmt.Sprintf("/v1/contracts/%s/parties", contract.ID)
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, publicPath, http.NoBody)
		req.Header.Set("X-User-Id", uuid.New().String())
		req.Header.Set("X-Kyc-Tier", "2")
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		// Must be 404 (no matching route) NOT 200.
		assert.Equal(t, http.StatusNotFound, w.Code,
			"roster endpoint must NOT be mounted on the public /v1 group")
	})
}

// TestRoster_GetParties_NonExistentContract verifies the phantom-contract guard:
// a valid service token + a UUID that does not exist in the DB must return 404,
// not 200 with an empty roster. Payment uses this endpoint to build settlement plans,
// so a phantom/unknown contract MUST NOT return a successful empty payload.
func TestRoster_GetParties_NonExistentContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping roster handler integration test in short mode")
	}

	ctx := context.Background()
	env := startRosterTestDB(t, ctx)

	// Use a random UUID that was never inserted into the DB.
	phantomID := uuid.New()
	path := fmt.Sprintf("/internal/v1/contracts/%s/parties", phantomID)

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	req.Header.Set("X-Service-Token", rosterTestServiceToken)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"roster of a non-existent contract must return 404, not 200 with empty []")
}
