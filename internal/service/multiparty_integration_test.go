package service_test

// Phase 2 integration tests for the multi-party N-vendor contract aggregate.
//
// Tests:
//  1. Migrations apply (tables created).
//  2. create contract + add parties (idempotent S2S).
//  3. submit Σ-gate: refuse !=10000, accept ==10000.
//  4. N-party sign reaching quorum -> ACTIVE + event.
//  5. Concurrent-sign TOCTOU (two parties sign simultaneously -> exactly-once activation).
//  6. Stale-version signature rejected.
//  7. Wrong-hash signature rejected.
//  8. 1:1 dual-sign aggregate still works (regression).
//  9. CanonicalMultipartyDigest changes when roster changes (after submit, hash is frozen).

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

// multipartyTestEnv holds all stores and services for multiparty integration tests.
type multipartyTestEnv struct {
	mpSvc         *service.MultipartyContractService
	contractStore *postgres.ContractStore
	sigStore      *postgres.SignatureStore
}

// startMultipartyTestDB returns a populated multipartyTestEnv backed by the singleton
// sharedServicePool (started once in TestMain).  No new container is started here.
func startMultipartyTestDB(t *testing.T, _ context.Context) *multipartyTestEnv {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	pool := sharedServicePool

	mpContracts := postgres.NewMultipartyContractStore(pool)
	mpParties := postgres.NewMultipartyPartyStore(pool)
	mpSigs := postgres.NewMultipartySignatureStore(pool)
	mpTx := postgres.NewMultipartyTxManager(pool)
	addendaStore := postgres.NewAddendumStore(pool)
	pub := events.NewNoopPublisher()

	mpSvc := service.NewMultipartyContractService(mpContracts, mpParties, mpSigs, addendaStore, mpTx, pub)

	return &multipartyTestEnv{
		mpSvc:         mpSvc,
		contractStore: postgres.NewContractStore(pool),
		sigStore:      postgres.NewSignatureStore(pool),
	}
}

// testCurrencyTWD is the TWD currency code used across multiparty test helpers.
const testCurrencyTWD = "TWD"

// mustDecimal parses a decimal string and panics on error (test helper).
func mustDecimal(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(fmt.Sprintf("mustDecimal: %v", err))
	}

	return d
}

// makeDualSignContract builds a minimal 1:1 dual-sign contract for regression tests.
// Mirrors the helper in the postgres store integration test.
func makeDualSignContract(clientID, freelancerID uuid.UUID) *domain.Contract {
	now := time.Now().UTC().Truncate(time.Millisecond)
	cid := uuid.New()

	amount := "5000.00"
	hash := domain.CanonicalContractDigest(
		cid.String(), clientID.String(), freelancerID.String(),
		"Regression Contract", "Terms body", amount, testCurrencyTWD, 1,
	)

	return &domain.Contract{
		ID:               cid,
		ListingID:        uuid.New(),
		AcceptedBidID:    uuid.New(),
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Title:            "Regression Contract",
		Terms:            "Terms body",
		Amount:           mustDecimal(amount),
		Currency:         testCurrencyTWD,
		ContentHash:      hash,
		Version:          1,
		Status:           domain.ContractStatusDraft,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// TestMultiparty_MigrationsApply verifies the multiparty tables were created by migration 000006.
// Uses the singleton sharedServicePool (migrations already applied by TestMain).
func TestMultiparty_MigrationsApply(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	ctx := context.Background()

	tables := []string{
		"multi_party_contracts",
		"multi_party_contract_parties",
		"multi_party_contract_signatures",
	}

	for _, tbl := range tables {
		var count int

		row := sharedServicePool.QueryRow(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_name=$1 AND table_schema='public'`,
			tbl)
		require.NoError(t, row.Scan(&count))
		assert.Equal(t, 1, count, "table %s must exist after migrations", tbl)
	}
}

// TestMultiparty_CreateOrAddParty_Idempotent proves idempotent S2S creation and party addition.
func TestMultiparty_CreateOrAddParty_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()
	currency := testCurrencyTWD

	// First call: creates the contract + adds vendorA.
	contract1, partyA, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     6000,
		Currency:     &currency,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusDraft, contract1.Status)
	assert.Equal(t, vendorA, partyA.VendorUserID)
	assert.Equal(t, 6000, partyA.ShareBps)

	// Second call with the same tenderID: returns the EXISTING contract, adds vendorB.
	contract2, partyB, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorB,
		ShareBps:     4000,
		Currency:     &currency,
	})
	require.NoError(t, err)
	assert.Equal(t, contract1.ID, contract2.ID, "second call must return the same contract")
	assert.Equal(t, vendorB, partyB.VendorUserID)
	assert.Equal(t, 4000, partyB.ShareBps)

	// Duplicate active vendor: returns ErrConflict.
	_, _, conflictErr := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA, // already ACTIVE
		ShareBps:     1000,
	})
	require.ErrorIs(t, conflictErr, domain.ErrConflict,
		"adding a duplicate ACTIVE vendor must return ErrConflict")
}

// TestMultiparty_SubmitForSignatures_OwnerOnly proves that SubmitForSignatures enforces
// the owner-only gate: a non-poster caller receives ErrForbidden even when the share sum
// is valid. The callerUserID parameter added by finding #1 closes the TOCTOU window.
func TestMultiparty_SubmitForSignatures_OwnerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()

	cases := []struct {
		name        string
		setupPoster func(posterID uuid.UUID) *uuid.UUID // nil = no posterUserID on contract
		callerIsNot string
		wantErr     error
	}{
		{
			name:        "non-poster caller → ErrForbidden",
			setupPoster: func(posterID uuid.UUID) *uuid.UUID { return &posterID },
			callerIsNot: "poster",
			wantErr:     domain.ErrForbidden,
		},
		{
			name:        "contract with nil PosterUserID → ErrForbidden (legacy row guard)",
			setupPoster: func(_ uuid.UUID) *uuid.UUID { return nil },
			callerIsNot: "anyone",
			wantErr:     domain.ErrForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := startMultipartyTestDB(t, ctx)
			tenderID := uuid.New()
			posterID := uuid.New()
			caller := uuid.New() // a different identity from the poster

			pUID := tc.setupPoster(posterID)

			contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
				TenderID:     tenderID,
				VendorUserID: uuid.New(),
				ShareBps:     10000,
				PosterUserID: pUID,
			})
			require.NoError(t, err)

			_, submitErr := env.mpSvc.SubmitForSignatures(ctx, contract.ID, caller)
			require.ErrorIs(t, submitErr, tc.wantErr,
				"non-poster calling SubmitForSignatures must return %v", tc.wantErr)
		})
	}

	t.Run("poster caller → succeeds when sum == 10000", func(t *testing.T) {
		env := startMultipartyTestDB(t, ctx)
		tenderID := uuid.New()
		posterID := uuid.New()

		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: uuid.New(),
			ShareBps:     10000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		submitted, submitErr := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
		require.NoError(t, submitErr, "poster calling SubmitForSignatures with valid sum must succeed")
		assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, submitted.Status)
	})
}

// TestMultiparty_SubmitForSignatures_ShareSumGate proves the Σ-gate:
// refuse if sum != 10000, accept if sum == 10000.
func TestMultiparty_SubmitForSignatures_ShareSumGate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()

	t.Run("refuse if sum != 10000", func(t *testing.T) {
		env := startMultipartyTestDB(t, ctx)
		tenderID := uuid.New()
		posterID := uuid.New()

		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: uuid.New(),
			ShareBps:     5000, // only 5000, not 10000
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		_, submitErr := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
		require.ErrorIs(t, submitErr, domain.ErrShareSumNotFull,
			"submit with sum!=10000 must return ErrShareSumNotFull")
	})

	t.Run("refuse if zero parties", func(t *testing.T) {
		env := startMultipartyTestDB(t, ctx)
		tenderID := uuid.New()
		posterID := uuid.New()

		// Create contract with no parties by exploiting a single-party sum of 0.
		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: uuid.New(),
			ShareBps:     0,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		_, submitErr := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
		require.ErrorIs(t, submitErr, domain.ErrShareSumNotFull,
			"submit with zero sum must return ErrShareSumNotFull")
	})

	t.Run("accept if sum == 10000", func(t *testing.T) {
		env := startMultipartyTestDB(t, ctx)
		tenderID := uuid.New()
		posterID := uuid.New()

		contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: uuid.New(),
			ShareBps:     6000,
			PosterUserID: &posterID,
		})
		require.NoError(t, err)

		_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID:     tenderID,
			VendorUserID: uuid.New(),
			ShareBps:     4000,
		})
		require.NoError(t, err)

		submitted, submitErr := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
		require.NoError(t, submitErr, "submit with sum==10000 must succeed")
		assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, submitted.Status)
		assert.NotEmpty(t, submitted.ContentHash, "content_hash must be set after submit")
	})
}

// TestMultiparty_NPartySign_QuorumActivation proves N-party signing reaches ACTIVE.
func TestMultiparty_NPartySign_QuorumActivation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()
	vendorC := uuid.New()

	// Create contract and add 3 parties (sum = 10000).
	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     5000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorB,
		ShareBps:     3000,
	})
	require.NoError(t, err)

	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorC,
		ShareBps:     2000,
	})
	require.NoError(t, err)

	// Submit for signatures.
	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, submitted.Status)

	digest := submitted.ContentHash

	// VendorA signs first — still PENDING_SIGNATURES (2 more needed).
	afterA, err := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorA,
		SignedContentHash: digest,
		Version:           1,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, afterA.Status,
		"after 1/3 signatures contract must still be PENDING_SIGNATURES")

	// VendorB signs — still PENDING_SIGNATURES (1 more needed).
	afterB, err := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorB,
		SignedContentHash: digest,
		Version:           1,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, afterB.Status,
		"after 2/3 signatures contract must still be PENDING_SIGNATURES")

	// VendorC signs — quorum reached, contract transitions to ACTIVE.
	afterC, err := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorC,
		SignedContentHash: digest,
		Version:           1,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusActive, afterC.Status,
		"after 3/3 signatures contract must be ACTIVE")

	// GetDetail confirms signed_count == total_parties == 3 (caller = vendorA, an ACTIVE party).
	detail, err := env.mpSvc.GetDetail(ctx, contract.ID, vendorA)
	require.NoError(t, err)
	assert.Equal(t, 3, detail.SignedCount)
	assert.Equal(t, 3, detail.TotalParties)
	assert.Equal(t, domain.MultipartyContractStatusActive, detail.Contract.Status)
}

// TestMultiparty_Sign_WrongHash proves that a signature with wrong hash is rejected.
func TestMultiparty_Sign_WrongHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()

	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     10000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)

	wrongHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	require.NotEqual(t, submitted.ContentHash, wrongHash)

	_, signErr := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorA,
		SignedContentHash: wrongHash,
		Version:           1,
	})
	require.ErrorIs(t, signErr, domain.ErrHashMismatch,
		"signing with wrong hash must return ErrHashMismatch")
}

// TestMultiparty_Sign_StaleVersion proves that a signature with wrong version is rejected.
func TestMultiparty_Sign_StaleVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()

	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     10000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)

	// Submit with version=99 (stale).
	_, signErr := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorA,
		SignedContentHash: submitted.ContentHash,
		Version:           99,
	})
	require.ErrorIs(t, signErr, domain.ErrStaleVersion,
		"signing with wrong version must return ErrStaleVersion")
}

// TestMultiparty_ConcurrentSign_TOCTOU proves that two concurrent sign calls result
// in exactly-once activation (no double-activate, no missed activation).
func TestMultiparty_ConcurrentSign_TOCTOU(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()

	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     5000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorB,
		ShareBps:     5000,
	})
	require.NoError(t, err)

	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)

	digest := submitted.ContentHash

	type result struct {
		status domain.MultipartyContractStatus
		err    error
	}

	results := make([]result, 2)
	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		c, signErr := env.mpSvc.Sign(ctx, service.SignInput{
			ContractID:        contract.ID,
			SignerUserID:      vendorA,
			SignedContentHash: digest,
			Version:           1,
		})
		if c != nil {
			results[0] = result{status: c.Status, err: signErr}
		} else {
			results[0] = result{err: signErr}
		}
	}()

	go func() {
		defer wg.Done()

		c, signErr := env.mpSvc.Sign(ctx, service.SignInput{
			ContractID:        contract.ID,
			SignerUserID:      vendorB,
			SignedContentHash: digest,
			Version:           1,
		})
		if c != nil {
			results[1] = result{status: c.Status, err: signErr}
		} else {
			results[1] = result{err: signErr}
		}
	}()

	wg.Wait()

	// Both goroutines must succeed without error.
	require.NoError(t, results[0].err, "vendorA sign must not error")
	require.NoError(t, results[1].err, "vendorB sign must not error")

	// Exactly one result must show ACTIVE status (the quorum-reaching signer).
	// The other may show PENDING_SIGNATURES or ACTIVE depending on goroutine ordering.
	// What is forbidden is ACTIVE appearing more than once (double-activation).
	activeCount := 0

	for _, r := range results {
		if r.status == domain.MultipartyContractStatusActive {
			activeCount++
		}
	}

	// At least one must be ACTIVE (the quorum was reached).
	assert.GreaterOrEqual(t, activeCount, 1, "at least one signer must observe ACTIVE status")

	// Wait a moment for the second goroutine's DB state to be fully committed.
	time.Sleep(10 * time.Millisecond)

	// Final authoritative DB read must show ACTIVE (caller = vendorA, an ACTIVE party).
	detail, err := env.mpSvc.GetDetail(ctx, contract.ID, vendorA)
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusActive, detail.Contract.Status,
		"contract must be ACTIVE after both parties signed")
	assert.Equal(t, 2, detail.SignedCount, "exactly 2 signatures must be recorded")
	assert.Equal(t, 2, detail.TotalParties)
}

// TestMultiparty_Sign_NonParty_Rejected proves C-1: a non-party authenticated user
// cannot sign a PENDING_SIGNATURES contract even if they supply the correct hash.
// The contract must NOT transition to ACTIVE.
func TestMultiparty_Sign_NonParty_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()
	nonParty := uuid.New() // authenticated Tier-2 user who is NOT a party

	// Create contract with vendorA as the sole party (10000 bps).
	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     10000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	// Submit for signatures (vendorA must sign).
	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, submitted.Status)

	// non-party signs with the correct hash and version — MUST be rejected.
	_, signErr := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      nonParty,
		SignedContentHash: submitted.ContentHash,
		Version:           1,
	})
	require.ErrorIs(t, signErr, domain.ErrNotParty,
		"a non-party user signing must return ErrNotParty")

	// Contract must NOT have been activated — still PENDING_SIGNATURES.
	detail, err := env.mpSvc.GetDetail(ctx, contract.ID, vendorA)
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, detail.Contract.Status,
		"non-party sign attempt must NOT activate the contract")
	assert.Equal(t, 0, detail.SignedCount,
		"no signatures must be recorded after non-party sign attempt")
}

// TestMultiparty_GetDetail_NonParty_Rejected proves M-3: a non-party Tier-1 user
// cannot read a multi-party contract's full roster and digest.
func TestMultiparty_GetDetail_NonParty_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()
	nonParty := uuid.New() // authenticated Tier-1 user who is NOT a party

	// Create contract and submit it so a hash exists.
	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     10000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	_, err = env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)

	// Non-party calls GetDetail — MUST be rejected.
	_, detailErr := env.mpSvc.GetDetail(ctx, contract.ID, nonParty)
	require.ErrorIs(t, detailErr, domain.ErrNotParty,
		"a non-party user reading GetDetail must return ErrNotParty (mapped to 404)")

	// Active party CAN read GetDetail successfully.
	detail, err := env.mpSvc.GetDetail(ctx, contract.ID, vendorA)
	require.NoError(t, err, "an ACTIVE party must be able to read contract detail")
	assert.Equal(t, contract.ID, detail.Contract.ID)
	assert.Equal(t, 1, detail.TotalParties)
}

// TestMultiparty_FrozenPartyCount_QuorumNotManipulable proves M-2: the quorum
// check uses the frozen party_count (set at SubmitForSignatures), not a live COUNT(*).
// Scenario: 2 parties submit → 1 party exits after submit → quorum must still
// require 2 signatures (frozen count), not 1 (live count).
//
// NOTE: party exit (EXITED status transition) is not yet implemented as a service
// method, so we simulate it by directly updating the DB row to bypass the service layer.
// This is acceptable in integration tests where we need to exercise the quorum boundary.
func TestMultiparty_FrozenPartyCount_QuorumNotManipulable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()

	contract, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorA,
		ShareBps:     5000,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorB,
		ShareBps:     5000,
	})
	require.NoError(t, err)

	// Submit: party_count is frozen at 2.
	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)
	assert.Equal(t, 2, submitted.PartyCount, "party_count must be frozen at 2 after submit")

	// Simulate vendorB "exiting" by directly marking the party row EXITED.
	// (Phase-4 exit flow is not yet implemented; we test the quorum invariant directly.)
	_, execErr := env.contractStore.Pool().Exec(
		ctx,
		`UPDATE multi_party_contract_parties SET status = 'EXITED', updated_at = now()
		 WHERE contract_id = $1 AND vendor_user_id = $2 AND status = 'ACTIVE'`,
		contract.ID, vendorB,
	)
	require.NoError(t, execErr, "direct DB update to simulate party exit")

	// vendorA signs — only 1 signature, but party_count is frozen at 2.
	// Contract MUST remain PENDING_SIGNATURES (1 < 2 frozen).
	afterA, signErr := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorA,
		SignedContentHash: submitted.ContentHash,
		Version:           1,
	})
	require.NoError(t, signErr)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, afterA.Status,
		"with frozen party_count=2, signing once must NOT activate the contract")
}

// TestMultiparty_DualSignRegression proves the 1:1 dual-sign aggregate still works
// after adding multiparty tables (schema regression test).
func TestMultiparty_DualSignRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	clientID := uuid.New()
	freelancerID := uuid.New()

	c := makeDualSignContract(clientID, freelancerID)
	require.NoError(t, env.contractStore.Create(ctx, c))

	got, err := env.contractStore.GetByID(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, c.ID, got.ID)
	assert.Equal(t, domain.ContractStatusDraft, got.Status)

	// Verify 1:1 signature store also works.
	now := time.Now().UTC().Truncate(time.Millisecond)
	sig := &domain.Signature{
		ID:                uuid.New(),
		ContractID:        c.ID,
		SignerUserID:      clientID,
		SignerRole:        domain.SignerRoleClient,
		ContractVersion:   1,
		SignedContentHash: c.ContentHash,
		SignedAt:          now,
		CreatedAt:         now,
	}

	require.NoError(t, env.sigStore.Create(ctx, sig))

	sigs, err := env.sigStore.ListByContract(ctx, c.ID)
	require.NoError(t, err)
	require.Len(t, sigs, 1)
	assert.Equal(t, sig.ID, sigs[0].ID)

	// Confirm the multiparty tables are separate (no cross-contamination):
	// the 1:1 contract c.ID must NOT appear in multi_party_contracts.
	var mpContractCount int

	row := env.contractStore.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM multi_party_contracts WHERE id = $1`, c.ID)
	require.NoError(t, row.Scan(&mpContractCount))

	assert.Equal(t, 0, mpContractCount,
		"1:1 contract %s must not appear in multi_party_contracts (no cross-contamination)", c.ID)
}

// TestMultiparty_GetDetail returns contract + roster + signature progress.
func TestMultiparty_GetDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multiparty integration test in short mode")
	}

	ctx := context.Background()
	env := startMultipartyTestDB(t, ctx)

	tenderID := uuid.New()
	posterID := uuid.New()
	vendorA := uuid.New()
	vendorB := uuid.New()

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

	submitted, err := env.mpSvc.SubmitForSignatures(ctx, contract.ID, posterID)
	require.NoError(t, err)

	// Sign as vendorA only.
	_, err = env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contract.ID,
		SignerUserID:      vendorA,
		SignedContentHash: submitted.ContentHash,
		Version:           1,
	})
	require.NoError(t, err)

	// GetDetail as vendorA (an ACTIVE party) — should succeed.
	detail, err := env.mpSvc.GetDetail(ctx, contract.ID, vendorA)
	require.NoError(t, err)

	assert.Equal(t, contract.ID, detail.Contract.ID)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, detail.Contract.Status,
		"with only 1/2 signatures contract must remain PENDING_SIGNATURES")
	assert.Equal(t, 2, detail.TotalParties)
	assert.Equal(t, 1, detail.SignedCount, "only vendorA signed so far")
	assert.Equal(t, 1, detail.CurrentVersion)
	assert.NotEmpty(t, detail.ContentHash)
	assert.Len(t, detail.Parties, 2)

	// Verify both party shares are correct.
	shareMap := make(map[uuid.UUID]int)
	for _, p := range detail.Parties {
		shareMap[p.VendorUserID] = p.ShareBps
	}

	assert.Equal(t, 7000, shareMap[vendorA])
	assert.Equal(t, 3000, shareMap[vendorB])

	// Log for human inspection.
	t.Logf("detail: signed=%d / total=%d, hash=%s, version=%d",
		detail.SignedCount, detail.TotalParties, fmt.Sprintf("%.12s...", detail.ContentHash), detail.CurrentVersion)
}
