package service_test

// Phase 4 integration tests for the multi-party N-vendor contract addendum + re-sign flow.
//
// Tests:
//  1. ValidMultipartyContractTransition correctness for P4 statuses (unit, no DB).
//  2. AddPartyToActive → status=ADDENDUM_PENDING, version bumped, party_count=3.
//  3. AddPartyToInvalidStatus (PENDING_SIGNATURES, ADDENDUM_PENDING) → ErrInvalidTransition.
//  4. Duplicate active vendor on ACTIVE contract → ErrConflict.
//  5. Non-owner add-party on ACTIVE → ErrForbidden.
//  6. DigestChangesWithNewRoster (unit, no DB).
//  7. UpdatePartyShare: invalid bps, non-owner, success, non-ADDENDUM_PENDING.
//  8. SubmitAfterAddendum: Σ≠10000 fails; Σ=10000 succeeds with frozen party_count.
//  9. FullReSignFlow: all 3 parties re-sign v2 hash → ACTIVE.
// 10. StaleVersionSign → ErrStaleVersion.
// 11. ConcurrentAddParty: exactly 1 success, 1 error.
// 12. AddendumRow: correct from/to version, new_vendor_user_id, triggered_by.
// 13. Migration000008_AppliesAndRollsBack.

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// p4Env groups service + addenda store for P4 tests.
type p4Env struct {
	svc     *service.MultipartyContractService
	addenda *postgres.AddendumStore
}

// startP4Env returns a p4Env backed by the singleton sharedServicePool
// (started once in TestMain).  No new container is started here.
func startP4Env(t *testing.T, _ context.Context) *p4Env {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	pool := sharedServicePool

	mpContracts := postgres.NewMultipartyContractStore(pool)
	mpParties := postgres.NewMultipartyPartyStore(pool)
	mpSigs := postgres.NewMultipartySignatureStore(pool)
	mpTx := postgres.NewMultipartyTxManager(pool)
	addendaStore := postgres.NewAddendumStore(pool)
	pub := events.NewNoopPublisher()

	svc := service.NewMultipartyContractService(mpContracts, mpParties, mpSigs, addendaStore, mpTx, pub)

	return &p4Env{svc: svc, addenda: addendaStore}
}

// activeContractFixtureResult groups return values of activeContractFixture.
type activeContractFixtureResult struct {
	contractID uuid.UUID
	tenderID   uuid.UUID
	vA         uuid.UUID
	vB         uuid.UUID
	poster     uuid.UUID
	v1Hash     string
}

// activeContractFixture creates a 2-party ACTIVE contract (vA=6000, vB=4000).
func activeContractFixture(
	t *testing.T, ctx context.Context, svc *service.MultipartyContractService,
) activeContractFixtureResult {
	t.Helper()

	tenderID := uuid.New()
	vA := uuid.New()
	vB := uuid.New()
	poster := uuid.New()
	currency := testCurrencyTWD

	c, _, err := svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vA,
		ShareBps:     6000,
		Currency:     &currency,
		PosterUserID: &poster,
	})
	require.NoError(t, err)

	_, _, err = svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vB,
		ShareBps:     4000,
		PosterUserID: &poster,
	})
	require.NoError(t, err)

	submitted, err := svc.SubmitForSignatures(ctx, c.ID, poster)
	require.NoError(t, err)

	v1Hash := submitted.ContentHash

	_, err = svc.Sign(ctx, service.SignInput{
		ContractID:        c.ID,
		SignerUserID:      vA,
		SignedContentHash: v1Hash,
		Version:           1,
	})
	require.NoError(t, err)

	active, err := svc.Sign(ctx, service.SignInput{
		ContractID:        c.ID,
		SignerUserID:      vB,
		SignedContentHash: v1Hash,
		Version:           1,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, active.Status)

	return activeContractFixtureResult{
		contractID: c.ID,
		tenderID:   tenderID,
		vA:         vA,
		vB:         vB,
		poster:     poster,
		v1Hash:     v1Hash,
	}
}

// TestP4_ValidTransitions verifies the updated state machine (unit, no DB).
func TestP4_ValidTransitions(t *testing.T) {
	cases := []struct {
		from    domain.MultipartyContractStatus
		to      domain.MultipartyContractStatus
		allowed bool
	}{
		// Existing transitions preserved.
		{domain.MultipartyContractStatusDraft, domain.MultipartyContractStatusPendingSignatures, true},
		{domain.MultipartyContractStatusDraft, domain.MultipartyContractStatusCancelled, true},
		{domain.MultipartyContractStatusPendingSignatures, domain.MultipartyContractStatusActive, true},
		{domain.MultipartyContractStatusPendingSignatures, domain.MultipartyContractStatusCancelled, true},
		{domain.MultipartyContractStatusActive, domain.MultipartyContractStatusCompleted, true},
		{domain.MultipartyContractStatusActive, domain.MultipartyContractStatusCancelled, true},
		// P4 additions.
		{domain.MultipartyContractStatusActive, domain.MultipartyContractStatusAddendumPending, true},
		{domain.MultipartyContractStatusAddendumPending, domain.MultipartyContractStatusPendingSignatures, true},
		{domain.MultipartyContractStatusAddendumPending, domain.MultipartyContractStatusCancelled, true},
		// Forbidden transitions.
		{domain.MultipartyContractStatusDraft, domain.MultipartyContractStatusActive, false},
		{domain.MultipartyContractStatusAddendumPending, domain.MultipartyContractStatusActive, false},
		{domain.MultipartyContractStatusAddendumPending, domain.MultipartyContractStatusCompleted, false},
		{domain.MultipartyContractStatusCompleted, domain.MultipartyContractStatusActive, false},
		{domain.MultipartyContractStatusCancelled, domain.MultipartyContractStatusDraft, false},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_to_%s", tc.from, tc.to), func(t *testing.T) {
			got := domain.ValidMultipartyContractTransition(tc.from, tc.to)
			assert.Equal(t, tc.allowed, got,
				"ValidMultipartyContractTransition(%s,%s) got %v want %v", tc.from, tc.to, got, tc.allowed)
		})
	}
}

// TestP4_AddPartyToActive verifies that adding a party to an ACTIVE contract
// creates the party at 0 bps, transitions to ADDENDUM_PENDING, and bumps the version.
func TestP4_AddPartyToActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix234 := activeContractFixture(t, ctx, env.svc)
	contractID := fix234.contractID
	tenderID := fix234.tenderID
	poster := fix234.poster
	v1Hash := fix234.v1Hash

	vC := uuid.New()

	afterAddendum, partyC, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vC,
		ShareBps:     0,
		PosterUserID: &poster,
	})
	require.NoError(t, err)

	assert.Equal(t, domain.MultipartyContractStatusAddendumPending, afterAddendum.Status,
		"contract must be ADDENDUM_PENDING after adding party to ACTIVE contract")
	assert.Equal(t, contractID, afterAddendum.ID)
	assert.Equal(t, 2, afterAddendum.Version, "version must be bumped from 1 to 2")
	assert.Equal(t, 3, afterAddendum.PartyCount, "party_count must be 3")
	assert.NotEqual(t, v1Hash, afterAddendum.ContentHash, "new digest must differ from v1 digest")
	assert.Equal(t, vC, partyC.VendorUserID)
	assert.Equal(t, 0, partyC.ShareBps, "placeholder party must have 0 bps")
}

// TestP4_AddPartyToInvalidStatus verifies ErrInvalidTransition for non-DRAFT/non-ACTIVE.
func TestP4_AddPartyToInvalidStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()

	t.Run("PENDING_SIGNATURES", func(t *testing.T) {
		env := startP4Env(t, ctx)
		poster := uuid.New()
		tid := uuid.New()

		c, _, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tid, VendorUserID: uuid.New(), ShareBps: 10000, PosterUserID: &poster,
		})
		require.NoError(t, err)

		_, err = env.svc.SubmitForSignatures(ctx, c.ID, poster)
		require.NoError(t, err)

		_, _, addErr := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tid, VendorUserID: uuid.New(), ShareBps: 0, PosterUserID: &poster,
		})
		require.ErrorIs(t, addErr, domain.ErrInvalidTransition)
	})

	t.Run("ADDENDUM_PENDING", func(t *testing.T) {
		env := startP4Env(t, ctx)
		fix285 := activeContractFixture(t, ctx, env.svc)
		tenderID := fix285.tenderID
		poster := fix285.poster

		// First add → ADDENDUM_PENDING.
		_, _, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: uuid.New(), ShareBps: 0, PosterUserID: &poster,
		})
		require.NoError(t, err)

		// Second add while already ADDENDUM_PENDING → ErrInvalidTransition.
		_, _, addErr := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: uuid.New(), ShareBps: 0, PosterUserID: &poster,
		})
		require.ErrorIs(t, addErr, domain.ErrInvalidTransition)
	})
}

// TestP4_DuplicateActiveVendorOnActiveContract verifies ErrConflict for duplicate vendor.
func TestP4_DuplicateActiveVendorOnActiveContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix310 := activeContractFixture(t, ctx, env.svc)
	tenderID := fix310.tenderID
	vA := fix310.vA
	poster := fix310.poster

	_, _, conflictErr := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID: tenderID, VendorUserID: vA, ShareBps: 0, PosterUserID: &poster,
	})
	require.ErrorIs(t, conflictErr, domain.ErrConflict,
		"duplicate active vendor on ACTIVE contract must return ErrConflict")
}

// TestP4_NonOwnerAddPartyToActive verifies ErrForbidden for non-owner.
func TestP4_NonOwnerAddPartyToActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix328 := activeContractFixture(t, ctx, env.svc)
	tenderID := fix328.tenderID

	notPoster := uuid.New()

	_, _, forbErr := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID: tenderID, VendorUserID: uuid.New(), ShareBps: 0, PosterUserID: &notPoster,
	})
	require.ErrorIs(t, forbErr, domain.ErrForbidden,
		"non-owner adding party to ACTIVE contract must return ErrForbidden")
}

// TestP4_DigestChangesWithNewRoster is a unit test (no DB) verifying digest properties.
func TestP4_DigestChangesWithNewRoster(t *testing.T) {
	tenderID := uuid.New()
	vA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	vB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	vC := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	currency := testCurrencyTWD

	v1Roster := []domain.MultipartyRosterEntry{
		{VendorUserID: vA, ShareBps: 6000},
		{VendorUserID: vB, ShareBps: 4000},
	}

	v2Roster0bps := []domain.MultipartyRosterEntry{
		{VendorUserID: vA, ShareBps: 6000},
		{VendorUserID: vB, ShareBps: 4000},
		{VendorUserID: vC, ShareBps: 0},
	}

	v2RosterFull := []domain.MultipartyRosterEntry{
		{VendorUserID: vA, ShareBps: 5000},
		{VendorUserID: vB, ShareBps: 4000},
		{VendorUserID: vC, ShareBps: 1000},
	}

	h1v1 := domain.CanonicalMultipartyDigest(tenderID, 1, currency, v1Roster)
	h1v2 := domain.CanonicalMultipartyDigest(tenderID, 2, currency, v1Roster)
	h2zero := domain.CanonicalMultipartyDigest(tenderID, 2, currency, v2Roster0bps)
	h2full := domain.CanonicalMultipartyDigest(tenderID, 2, currency, v2RosterFull)

	assert.NotEqual(t, h1v1, h1v2, "version bump must change the digest")
	assert.NotEqual(t, h1v1, h2zero, "adding 0-bps party must change the digest vs v1")
	assert.NotEqual(t, h1v2, h2zero, "same version, different roster must differ")
	assert.NotEqual(t, h2zero, h2full, "updating shares must change the digest")
}

// TestP4_UpdatePartyShare verifies PATCH share semantics.
func TestP4_UpdatePartyShare(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()

	t.Run("invalid bps 10001 → ErrValidation", func(t *testing.T) {
		env := startP4Env(t, ctx)
		fix385 := activeContractFixture(t, ctx, env.svc)
		contractID := fix385.contractID
		tenderID := fix385.tenderID
		poster := fix385.poster

		_, partyC, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: uuid.New(), ShareBps: 0, PosterUserID: &poster,
		})
		require.NoError(t, err)

		_, updateErr := env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
			ContractID: contractID, PartyID: partyC.ID, CallerUserID: poster, NewShareBps: 10001,
		})
		require.ErrorIs(t, updateErr, domain.ErrValidation)
	})

	t.Run("non-owner → ErrForbidden", func(t *testing.T) {
		env := startP4Env(t, ctx)
		fix400 := activeContractFixture(t, ctx, env.svc)
		contractID := fix400.contractID
		tenderID := fix400.tenderID
		poster := fix400.poster

		_, partyC, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: uuid.New(), ShareBps: 0, PosterUserID: &poster,
		})
		require.NoError(t, err)

		notPoster := uuid.New()
		_, updateErr := env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
			ContractID: contractID, PartyID: partyC.ID, CallerUserID: notPoster, NewShareBps: 1000,
		})
		require.ErrorIs(t, updateErr, domain.ErrForbidden)
	})

	t.Run("ADDENDUM_PENDING → success", func(t *testing.T) {
		env := startP4Env(t, ctx)
		fix416 := activeContractFixture(t, ctx, env.svc)
		contractID := fix416.contractID
		tenderID := fix416.tenderID
		poster := fix416.poster

		vC := uuid.New()
		_, partyC, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: vC, ShareBps: 0, PosterUserID: &poster,
		})
		require.NoError(t, err)

		updated, updateErr := env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
			ContractID: contractID, PartyID: partyC.ID, CallerUserID: poster, NewShareBps: 2000,
		})
		require.NoError(t, updateErr)
		assert.Equal(t, 2000, updated.ShareBps)
		assert.Equal(t, vC, updated.VendorUserID)
	})

	t.Run("DRAFT → ErrInvalidTransition", func(t *testing.T) {
		env := startP4Env(t, ctx)
		poster := uuid.New()

		draftC, draftParty, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: uuid.New(), VendorUserID: uuid.New(), ShareBps: 10000, PosterUserID: &poster,
		})
		require.NoError(t, err)
		require.Equal(t, domain.MultipartyContractStatusDraft, draftC.Status)

		_, updateErr := env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
			ContractID: draftC.ID, PartyID: draftParty.ID, CallerUserID: poster, NewShareBps: 5000,
		})
		require.ErrorIs(t, updateErr, domain.ErrInvalidTransition)
	})
}

// TestP4_SubmitAfterAddendum verifies Σ-gate on re-submit + frozen party_count.
func TestP4_SubmitAfterAddendum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix458 := activeContractFixture(t, ctx, env.svc)
	contractID := fix458.contractID
	tenderID := fix458.tenderID
	vA := fix458.vA
	vB := fix458.vB
	poster := fix458.poster
	v1Hash := fix458.v1Hash

	// Add vC (placeholder 0 bps) → ADDENDUM_PENDING.
	vC := uuid.New()
	addPending, partyC, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID: tenderID, VendorUserID: vC, ShareBps: 0, PosterUserID: &poster,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusAddendumPending, addPending.Status)

	// Fetch party IDs for vA.
	detail, err := env.svc.GetDetail(ctx, contractID, vA)
	require.NoError(t, err)

	partyIDs := make(map[uuid.UUID]uuid.UUID)
	for _, p := range detail.Parties {
		partyIDs[p.VendorUserID] = p.ID
	}

	t.Run("Σ≠10000 fails", func(t *testing.T) {
		// Patch vA to 5000 → sum = 5000+4000+0 = 9000.
		_, patchErr := env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
			ContractID: contractID, PartyID: partyIDs[vA], CallerUserID: poster, NewShareBps: 5000,
		})
		require.NoError(t, patchErr)

		_, submitErr := env.svc.SubmitForSignatures(ctx, contractID, poster)
		require.ErrorIs(t, submitErr, domain.ErrShareSumNotFull)

		// Restore vA to 6000 for next sub-test.
		_, _ = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
			ContractID: contractID, PartyID: partyIDs[vA], CallerUserID: poster, NewShareBps: 6000,
		})
	})

	t.Run("Σ=10000 succeeds", func(t *testing.T) {
		// vA=6000, vB=4000, vC=0 → sum=10000.
		resubmitted, submitErr := env.svc.SubmitForSignatures(ctx, contractID, poster)
		require.NoError(t, submitErr)
		assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, resubmitted.Status)
		assert.NotEmpty(t, resubmitted.ContentHash)
		assert.Equal(t, 3, resubmitted.PartyCount, "party_count must be frozen at 3")
		assert.NotEqual(t, v1Hash, resubmitted.ContentHash, "v2 digest must differ from v1 digest")
	})

	// Suppress unused-variable warnings in case t.Run scoping changes.
	_ = partyC
	_ = vB
}

// TestP4_FullReSignFlow verifies the complete flow: addendum → patch → resubmit → all re-sign → ACTIVE.
func TestP4_FullReSignFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix517 := activeContractFixture(t, ctx, env.svc)
	contractID := fix517.contractID
	tenderID := fix517.tenderID
	vA := fix517.vA
	vB := fix517.vB
	poster := fix517.poster
	v1Hash := fix517.v1Hash

	// Add vC via addendum → ADDENDUM_PENDING, version=2.
	vC := uuid.New()
	addPending, partyC, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID: tenderID, VendorUserID: vC, ShareBps: 0, PosterUserID: &poster,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusAddendumPending, addPending.Status)
	require.Equal(t, 2, addPending.Version)

	// Adjust shares: vA=5000, vB=4000, vC=1000 → sum=10000.
	detail, err := env.svc.GetDetail(ctx, contractID, vA)
	require.NoError(t, err)

	partyIDs := make(map[uuid.UUID]uuid.UUID)
	for _, p := range detail.Parties {
		partyIDs[p.VendorUserID] = p.ID
	}

	_, err = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID: contractID, PartyID: partyIDs[vA], CallerUserID: poster, NewShareBps: 5000,
	})
	require.NoError(t, err)

	_, err = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID: contractID, PartyID: partyIDs[vB], CallerUserID: poster, NewShareBps: 4000,
	})
	require.NoError(t, err)

	_, err = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID: contractID, PartyID: partyC.ID, CallerUserID: poster, NewShareBps: 1000,
	})
	require.NoError(t, err)

	// Re-submit: ADDENDUM_PENDING → PENDING_SIGNATURES (owner-only gate).
	resubmitted, err := env.svc.SubmitForSignatures(ctx, contractID, poster)
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, resubmitted.Status)
	assert.Equal(t, 2, resubmitted.Version)
	v2Hash := resubmitted.ContentHash
	assert.NotEmpty(t, v2Hash)
	assert.NotEqual(t, v1Hash, v2Hash, "v2 digest must differ from v1 digest")

	// Stale version sign → ErrStaleVersion.
	_, staleErr := env.svc.Sign(ctx, service.SignInput{
		ContractID: contractID, SignerUserID: vA, SignedContentHash: v2Hash, Version: 1,
	})
	require.ErrorIs(t, staleErr, domain.ErrStaleVersion)

	// All 3 parties re-sign with version=2.
	afterA, err := env.svc.Sign(ctx, service.SignInput{
		ContractID: contractID, SignerUserID: vA, SignedContentHash: v2Hash, Version: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, afterA.Status,
		"after 1/3 re-signs contract must still be PENDING_SIGNATURES")

	afterB, err := env.svc.Sign(ctx, service.SignInput{
		ContractID: contractID, SignerUserID: vB, SignedContentHash: v2Hash, Version: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusPendingSignatures, afterB.Status,
		"after 2/3 re-signs contract must still be PENDING_SIGNATURES")

	afterC, err := env.svc.Sign(ctx, service.SignInput{
		ContractID: contractID, SignerUserID: vC, SignedContentHash: v2Hash, Version: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MultipartyContractStatusActive, afterC.Status,
		"after 3/3 re-signs contract must be ACTIVE")
}

// TestP4_ConcurrentAddParty verifies exactly-once ADDENDUM_PENDING on concurrent calls.
func TestP4_ConcurrentAddParty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix599 := activeContractFixture(t, ctx, env.svc)
	tenderID := fix599.tenderID
	poster := fix599.poster

	vC := uuid.New()
	vD := uuid.New()

	type result struct {
		contract *domain.MultipartyContract
		err      error
	}

	results := make([]result, 2)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		c, _, addErr := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: vC, ShareBps: 0, PosterUserID: &poster,
		})
		results[0] = result{contract: c, err: addErr}
	}()

	go func() {
		defer wg.Done()

		c, _, addErr := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
			TenderID: tenderID, VendorUserID: vD, ShareBps: 0, PosterUserID: &poster,
		})
		results[1] = result{contract: c, err: addErr}
	}()

	wg.Wait()

	successCount, errCount := 0, 0

	for _, r := range results {
		if r.err == nil {
			successCount++
		} else {
			errCount++
		}
	}

	assert.Equal(t, 1, successCount, "exactly one concurrent add-party must succeed")
	assert.Equal(t, 1, errCount, "exactly one concurrent add-party must fail")

	// wg.Wait() above already joined all goroutines; the addendum row is written inside
	// the transaction before the goroutine returns, so no sleep is needed here.
	for _, r := range results {
		if r.err == nil {
			assert.Equal(t, domain.MultipartyContractStatusAddendumPending, r.contract.Status,
				"the successful add must result in ADDENDUM_PENDING")
		}
	}
}

// TestP4_AddendumRow verifies that the contract_addenda row has correct fields.
func TestP4_AddendumRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix667 := activeContractFixture(t, ctx, env.svc)
	contractID := fix667.contractID
	tenderID := fix667.tenderID
	poster := fix667.poster

	vC := uuid.New()
	afterAddendum, _, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID: tenderID, VendorUserID: vC, ShareBps: 0, PosterUserID: &poster,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusAddendumPending, afterAddendum.Status)

	// The addendum row is written inside the transaction before CreateOrAddParty returns;
	// no sleep needed — the row is guaranteed to be visible to subsequent reads.
	addenda, listErr := env.addenda.ListByContract(ctx, contractID)
	require.NoError(t, listErr)
	require.Len(t, addenda, 1, "exactly one addendum row must be recorded")

	a := addenda[0]
	assert.Equal(t, contractID, a.ContractID)
	assert.Equal(t, 1, a.FromVersion, "from_version must be 1 (pre-addendum version)")
	assert.Equal(t, 2, a.ToVersion, "to_version must be 2 (new version)")
	assert.Equal(t, vC, a.NewVendorUserID, "new_vendor_user_id must be vC")
	assert.Equal(t, poster, a.TriggeredBy, "triggered_by must be the poster")
	assert.Equal(t, afterAddendum.Version, a.ToVersion)
	assert.False(t, a.CreatedAt.IsZero(), "created_at must be set")
}

// TestP4_UpdatePartyShare_AfterSubmit_ErrInvalidTransition verifies the M1 TOCTOU fix:
// UpdatePartyShare on a contract that has transitioned to PENDING_SIGNATURES returns
// ErrInvalidTransition, not a silent success that would land a stale share patch on a
// contract whose digest is already frozen.
func TestP4_UpdatePartyShare_AfterSubmit_ErrInvalidTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 integration test in short mode")
	}

	ctx := context.Background()
	env := startP4Env(t, ctx)

	fix := activeContractFixture(t, ctx, env.svc)
	contractID := fix.contractID
	tenderID := fix.tenderID
	vA := fix.vA
	poster := fix.poster

	// Add vC via addendum → ADDENDUM_PENDING.
	vC := uuid.New()
	addPending, _, err := env.svc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID: tenderID, VendorUserID: vC, ShareBps: 0, PosterUserID: &poster,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusAddendumPending, addPending.Status)

	// Fetch party IDs so we can call UpdatePartyShare.
	detail, err := env.svc.GetDetail(ctx, contractID, vA)
	require.NoError(t, err)

	partyIDs := make(map[uuid.UUID]uuid.UUID)
	for _, p := range detail.Parties {
		partyIDs[p.VendorUserID] = p.ID
	}

	// Set shares to a valid sum: vA=5000, vB=4000, vC=1000.
	_, err = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID: contractID, PartyID: partyIDs[vA], CallerUserID: poster, NewShareBps: 5000,
	})
	require.NoError(t, err)

	_, err = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID: contractID, PartyID: partyIDs[fix.vB], CallerUserID: poster, NewShareBps: 4000,
	})
	require.NoError(t, err)

	_, err = env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID: contractID, PartyID: partyIDs[vC], CallerUserID: poster, NewShareBps: 1000,
	})
	require.NoError(t, err)

	// SubmitForSignatures transitions the contract to PENDING_SIGNATURES and freezes the digest (owner-only gate).
	_, err = env.svc.SubmitForSignatures(ctx, contractID, poster)
	require.NoError(t, err)

	// Now the contract is PENDING_SIGNATURES. A subsequent UpdatePartyShare must return
	// ErrInvalidTransition — the frozen digest must not be invalidated by a late PATCH.
	_, updateErr := env.svc.UpdatePartyShare(ctx, service.UpdatePartyShareInput{
		ContractID:   contractID,
		PartyID:      partyIDs[vA],
		CallerUserID: poster,
		NewShareBps:  9999,
	})
	require.ErrorIs(t, updateErr, domain.ErrInvalidTransition,
		"UpdatePartyShare on PENDING_SIGNATURES contract must return ErrInvalidTransition (M1 TOCTOU fix)")
}

// TestP4_Migration000008_AppliesAndRollsBack verifies migration 000008 up + down.
func TestP4_Migration000008_AppliesAndRollsBack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping P4 migration test in short mode")
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
		require.NoError(t, readErr, "read migration %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s", file)
	}

	// Verify contract_addenda table exists after 000008 up.
	var addendaCount int

	row := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name='contract_addenda' AND table_schema='public'`)
	require.NoError(t, row.Scan(&addendaCount))
	assert.Equal(t, 1, addendaCount, "contract_addenda table must exist after 000008 up")

	// Verify ADDENDUM_PENDING is now valid by inserting and then deleting a test row.
	// The row must be deleted before the down migration restores the 5-value CHECK,
	// otherwise the rollback fails because the test row violates the restored constraint.
	_, insertErr := pool.Exec(ctx,
		`INSERT INTO multi_party_contracts (id, tender_id, status, content_hash, version, party_count)
		 VALUES (gen_random_uuid(), gen_random_uuid(), 'ADDENDUM_PENDING', '', 1, 0)`)
	require.NoError(t, insertErr, "ADDENDUM_PENDING must be valid after 000008 up")

	// Delete the ADDENDUM_PENDING row before rolling back (the down migration
	// restores the 5-value CHECK which would reject this row).
	_, delErr := pool.Exec(ctx, `DELETE FROM multi_party_contracts WHERE status = 'ADDENDUM_PENDING'`)
	require.NoError(t, delErr, "must be able to clean up ADDENDUM_PENDING rows before rollback")

	// Roll back 000008.
	downData, readErr := migrations.FS.ReadFile("000008_p4_addendum.down.sql")
	require.NoError(t, readErr, "read 000008 down migration")

	_, downErr := pool.Exec(ctx, string(downData))
	require.NoError(t, downErr, "000008 down migration must apply cleanly")

	// Verify contract_addenda is gone after down.
	row = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name='contract_addenda' AND table_schema='public'`)
	require.NoError(t, row.Scan(&addendaCount))
	assert.Equal(t, 0, addendaCount, "contract_addenda table must be gone after 000008 down")

	// Verify ADDENDUM_PENDING is rejected after rollback.
	_, insertErr2 := pool.Exec(ctx,
		`INSERT INTO multi_party_contracts (id, tender_id, status, content_hash, version, party_count)
		 VALUES (gen_random_uuid(), gen_random_uuid(), 'ADDENDUM_PENDING', '', 1, 0)`)
	assert.Error(t, insertErr2, "ADDENDUM_PENDING must be rejected after 000008 down rollback")
}
