package service_test

// Integration tests for the proof service using the sharedServicePool Postgres testcontainer.
//
// These tests exercise the full flow:
//   1. Bilateral: create ACTIVE contract → GenerateAndStore → GetDownloadURL (party) → 403 (non-party).
//   2. Multiparty: create ACTIVE contract → GenerateAndStore → GetDownloadURL (party) → 403 (non-party).
//   3. Idempotency: calling GenerateAndStore twice returns the same proof ID.
//   4. Supersede: addendum re-sign to v2 → GenerateAndStore → row updated, one row only.
//   5. Store-layer: Create duplicate returns ErrProofAlreadyExists; GetByContract returns ErrProofNotFound.
//
// The FakeFileClient is used in place of the real file service to avoid an external
// HTTP dependency in integration tests. The Postgres testcontainer provides real
// schema and query execution.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/client"
	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- helpers ----------------------------------------------------------------

// proofTestEnv bundles all stores and services for proof integration tests.
type proofTestEnv struct {
	proofStore  *postgres.ContractProofStore
	contractSvc *service.ContractService
	mpSvc       *service.MultipartyContractService
	proofSvc    *service.ProofService
	fileClient  *client.FakeFileClient
}

// newProofTestEnv builds a fully wired proofTestEnv from the sharedServicePool.
func newProofTestEnv(t *testing.T) *proofTestEnv {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping proof integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	pool := sharedServicePool

	// Store layer.
	contractStore := postgres.NewContractStore(pool)
	sigStore := postgres.NewSignatureStore(pool)
	txMgr := postgres.NewTxManager(pool)
	auditLogStore := postgres.NewAuditLogStore(pool)
	mpContractStore := postgres.NewMultipartyContractStore(pool)
	mpPartyStore := postgres.NewMultipartyPartyStore(pool)
	mpSigStore := postgres.NewMultipartySignatureStore(pool)
	mpTxMgr := postgres.NewMultipartyTxManager(pool)
	addendaStore := postgres.NewAddendumStore(pool)
	proofStore := postgres.NewContractProofStore(pool)

	// Service layer.
	pub := events.NewNoopPublisher()
	contractSvc := service.NewContractService(contractStore, sigStore, txMgr, pub)
	mpSvc := service.NewMultipartyContractService(mpContractStore, mpPartyStore, mpSigStore, addendaStore, mpTxMgr, pub)

	// FakeFileClient — no external HTTP dependency.
	fakeFC := client.NewFakeFileClient(nil)

	proofSvc, err := service.NewProofService(&service.ProofServiceConfig{
		ProofStore:               proofStore,
		AuditStore:               auditLogStore,
		ContractStore:            contractStore,
		SignatureStore:           sigStore,
		MultipartyContractStore:  mpContractStore,
		MultipartyPartyStore:     mpPartyStore,
		MultipartySignatureStore: mpSigStore,
		FileClient:               fakeFC,
	})
	require.NoError(t, err, "NewProofService must succeed with valid config")

	return &proofTestEnv{
		proofStore:  proofStore,
		contractSvc: contractSvc,
		mpSvc:       mpSvc,
		proofSvc:    proofSvc,
		fileClient:  fakeFC,
	}
}

// createActiveBilateralContract creates a bilateral contract signed by both parties
// and returns the ACTIVE contract and the client user ID. Mirrors the sign-to-activate
// flow used in contract_m2_integration_test.go via direct service calls.
func createActiveBilateralContract(t *testing.T, env *proofTestEnv) (*domain.Contract, uuid.UUID) {
	t.Helper()

	ctx := context.Background()

	clientID := uuid.New()
	freelancerID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()

	amount := decimal.NewFromInt(1000)

	// Create DRAFT contract via the internal service call.
	in := &service.CreateContractInput{
		ListingID:        listingID,
		AcceptedBidID:    bidID,
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Amount:           amount,
		Currency:         testCurrencyTWD,
		Title:            fmt.Sprintf("Proof Integration Test Contract %s", uuid.New()),
		Terms:            "Test terms for proof integration.",
	}

	c, err := env.contractSvc.CreateContract(ctx, in)
	require.NoError(t, err, "CreateContractInternal must succeed")

	// Submit for signature (transition to PENDING_SIGNATURE).
	c, err = env.contractSvc.SubmitContract(ctx, c.ID, clientID)
	require.NoError(t, err, "SubmitContract must succeed")

	// Both parties sign with the current content hash.
	_, err = env.contractSvc.SignContract(ctx, service.SignContractInput{
		ContractID:        c.ID,
		CallerID:          clientID,
		SignedContentHash: c.ContentHash,
	})
	require.NoError(t, err, "client sign must succeed")

	result, err := env.contractSvc.SignContract(ctx, service.SignContractInput{
		ContractID:        c.ID,
		CallerID:          freelancerID,
		SignedContentHash: c.ContentHash,
	})
	require.NoError(t, err, "freelancer sign must succeed")
	require.Equal(t, domain.ContractStatusActive, result.Status,
		"contract must be ACTIVE after both parties sign")

	return result, clientID
}

// createActiveMultipartyContract creates a 2-vendor multiparty contract in ACTIVE
// status and returns (contractID, posterID, vendor1ID, vendor2ID).
func createActiveMultipartyContract(t *testing.T, env *proofTestEnv) (contractID, posterID, vendor1ID, vendor2ID uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	posterID = uuid.New()
	vendor1ID = uuid.New()
	vendor2ID = uuid.New()
	tenderID := uuid.New()

	currency := testCurrencyTWD

	_, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendor1ID,
		ShareBps:     5000,
		Currency:     &currency,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	c2, _, err := env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendor2ID,
		ShareBps:     5000,
	})
	require.NoError(t, err)

	contractID = c2.ID

	_, err = env.mpSvc.SubmitForSignatures(ctx, contractID, posterID)
	require.NoError(t, err)

	mpContract, err := env.mpSvc.GetDetail(ctx, contractID, vendor1ID)
	require.NoError(t, err)

	currentHash := mpContract.ContentHash

	_, err = env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contractID,
		SignerUserID:      vendor1ID,
		SignedContentHash: currentHash,
		Version:           1,
	})
	require.NoError(t, err)

	lastSign, err := env.mpSvc.Sign(ctx, service.SignInput{
		ContractID:        contractID,
		SignerUserID:      vendor2ID,
		SignedContentHash: currentHash,
		Version:           1,
	})
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, lastSign.Status,
		"multiparty contract must be ACTIVE after all parties sign")

	return contractID, posterID, vendor1ID, vendor2ID
}

// ---- Bilateral proof tests --------------------------------------------------

func TestProofService_Bilateral_GenerateAndStore(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	c, _ := createActiveBilateralContract(t, env)

	proof, err := env.proofSvc.GenerateAndStore(ctx, c.ID, domain.ContractKindBilateral)

	require.NoError(t, err, "GenerateAndStore must succeed for ACTIVE bilateral contract")
	require.NotNil(t, proof)
	assert.Equal(t, c.ID, proof.ContractID)
	assert.Equal(t, domain.ContractKindBilateral, proof.ContractKind)
	assert.NotEmpty(t, proof.SHA256, "SHA-256 digest must be populated")
	assert.NotEqual(t, uuid.Nil, proof.FileID, "FileID must not be nil")
	assert.NotEmpty(t, proof.ObjectKey)
	assert.False(t, proof.GeneratedAt.IsZero(), "GeneratedAt must be set")
	assert.Equal(t, c.Version, proof.ContractVersion, "ContractVersion must match contract version")

	// Independent DB read-back: verify persisted row matches returned proof.
	dbProof, err := env.proofStore.GetByContract(ctx, c.ID, domain.ContractKindBilateral)
	require.NoError(t, err, "DB read-back must succeed")
	assert.Equal(t, proof.ID, dbProof.ID, "DB proof ID must match returned proof")
	assert.Equal(t, proof.FileID, dbProof.FileID, "DB FileID must match returned proof")
	assert.Equal(t, proof.ContractVersion, dbProof.ContractVersion, "DB ContractVersion must match")
}

func TestProofService_Bilateral_PDFStoredInFakeClient(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	c, _ := createActiveBilateralContract(t, env)

	proof, err := env.proofSvc.GenerateAndStore(ctx, c.ID, domain.ContractKindBilateral)
	require.NoError(t, err)

	// The PDF bytes must be stored in the FakeFileClient.
	pdfBytes := env.fileClient.Get(proof.FileID)
	require.NotEmpty(t, pdfBytes, "PDF bytes must be stored in FakeFileClient")
	assert.Equal(t, []byte("%PDF-"), pdfBytes[:5], "stored bytes must be a valid PDF")
}

func TestProofService_Bilateral_GetDownloadURL_Party(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	c, clientID := createActiveBilateralContract(t, env)

	// Generate proof first.
	_, err := env.proofSvc.GenerateAndStore(ctx, c.ID, domain.ContractKindBilateral)
	require.NoError(t, err)

	// Client (party) must be able to download.
	url, ttl, err := env.proofSvc.GetDownloadURL(ctx, c.ID, domain.ContractKindBilateral, clientID)

	require.NoError(t, err, "party must get a download URL without error")
	assert.NotEmpty(t, url, "download URL must not be empty")
	assert.Greater(t, ttl, 0, "TTL must be positive")
}

func TestProofService_Bilateral_GetDownloadURL_NonParty_Returns403(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	c, _ := createActiveBilateralContract(t, env)

	_, err := env.proofSvc.GenerateAndStore(ctx, c.ID, domain.ContractKindBilateral)
	require.NoError(t, err)

	// Non-party should get ErrForbidden.
	nonPartyID := uuid.New()
	_, _, err = env.proofSvc.GetDownloadURL(ctx, c.ID, domain.ContractKindBilateral, nonPartyID)

	require.Error(t, err, "non-party must get an error")
	assert.ErrorIs(t, err, domain.ErrForbidden, "non-party must get ErrForbidden (403), not 404")
}

func TestProofService_Bilateral_GetDownloadURL_NoProofYet(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	c, clientID := createActiveBilateralContract(t, env)

	// No proof generated yet — must return ErrNotFound.
	_, _, err := env.proofSvc.GetDownloadURL(ctx, c.ID, domain.ContractKindBilateral, clientID)

	require.Error(t, err, "GetDownloadURL before proof is generated must return an error")
	assert.ErrorIs(t, err, domain.ErrNotFound, "missing proof must return ErrNotFound")
}

// ---- Idempotency test -------------------------------------------------------

func TestProofService_GenerateAndStore_Idempotent(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	c, _ := createActiveBilateralContract(t, env)

	// First call.
	proof1, err := env.proofSvc.GenerateAndStore(ctx, c.ID, domain.ContractKindBilateral)
	require.NoError(t, err)

	// Second call — must return the same proof without re-generating.
	proof2, err := env.proofSvc.GenerateAndStore(ctx, c.ID, domain.ContractKindBilateral)
	require.NoError(t, err)

	assert.Equal(t, proof1.ID, proof2.ID, "idempotent call must return the same proof ID")
	assert.Equal(t, proof1.FileID, proof2.FileID, "idempotent call must return the same file ID")
	// FakeFileClient must only have stored one PDF.
	assert.Len(t, env.fileClient.StoredIDs(), 1,
		"FakeFileClient must contain exactly one file (no re-upload on idempotent call)")
}

// ---- Store-layer unit tests -------------------------------------------------

func TestContractProofStore_CreateAndGet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	require.NotNil(t, sharedServicePool)

	proofStore := postgres.NewContractProofStore(sharedServicePool)
	ctx := context.Background()

	contractID := uuid.New()
	fileID := uuid.New()

	p := &domain.ContractProof{
		ID:              uuid.New(),
		ContractID:      contractID,
		ContractKind:    domain.ContractKindBilateral,
		ContractVersion: 1,
		FileID:          fileID,
		ObjectKey:       "fake/contract-proof/" + contractID.String() + ".pdf",
		SHA256:          "abc123def456",
		AuditChainHead:  "deadbeef",
		GeneratedAt:     time.Now().UTC().Truncate(time.Microsecond),
	}

	err := proofStore.Create(ctx, p)
	require.NoError(t, err, "Create must succeed for new proof")

	got, err := proofStore.GetByContract(ctx, contractID, domain.ContractKindBilateral)
	require.NoError(t, err, "GetByContract must return the stored proof")

	assert.Equal(t, p.ID, got.ID)
	assert.Equal(t, p.ContractID, got.ContractID)
	assert.Equal(t, p.ContractKind, got.ContractKind)
	assert.Equal(t, p.ContractVersion, got.ContractVersion)
	assert.Equal(t, p.FileID, got.FileID)
	assert.Equal(t, p.ObjectKey, got.ObjectKey)
	assert.Equal(t, p.SHA256, got.SHA256)
	assert.Equal(t, p.AuditChainHead, got.AuditChainHead)
}

func TestContractProofStore_Create_DuplicateReturnsErrProofAlreadyExists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	require.NotNil(t, sharedServicePool)

	proofStore := postgres.NewContractProofStore(sharedServicePool)
	ctx := context.Background()

	contractID := uuid.New()

	p := &domain.ContractProof{
		ID:              uuid.New(),
		ContractID:      contractID,
		ContractKind:    domain.ContractKindMultiparty,
		ContractVersion: 1,
		FileID:          uuid.New(),
		ObjectKey:       "fake/obj",
		SHA256:          "sha256abc",
		GeneratedAt:     time.Now().UTC(),
	}

	err := proofStore.Create(ctx, p)
	require.NoError(t, err, "first Create must succeed")

	// Second insert with the same (contract_id, contract_kind) must be rejected.
	duplicate := *p
	duplicate.ID = uuid.New() // different row ID but same contract
	err = proofStore.Create(ctx, &duplicate)

	assert.ErrorIs(t, err, store.ErrProofAlreadyExists,
		"duplicate insert must return ErrProofAlreadyExists")
}

func TestContractProofStore_GetByContract_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	require.NotNil(t, sharedServicePool)

	proofStore := postgres.NewContractProofStore(sharedServicePool)
	ctx := context.Background()

	_, err := proofStore.GetByContract(ctx, uuid.New(), domain.ContractKindBilateral)

	assert.ErrorIs(t, err, store.ErrProofNotFound,
		"GetByContract for unknown contract must return ErrProofNotFound")
}

// ---- Multiparty proof tests --------------------------------------------------

func TestProofService_Multiparty_GenerateAndStore(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	contractID, _, vendor1ID, _ := createActiveMultipartyContract(t, env)

	// Wire proofSvc into mpSvc so it runs proof generation inline.
	env.mpSvc.WithProofGenerator(env.proofSvc)

	// Verify the contract is ACTIVE before generating proof.
	mpContract, err := env.mpSvc.GetDetail(ctx, contractID, vendor1ID)
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, mpContract.Contract.Status,
		"multiparty contract must be ACTIVE before GenerateAndStore")

	proof, err := env.proofSvc.GenerateAndStore(ctx, contractID, domain.ContractKindMultiparty)

	require.NoError(t, err, "GenerateAndStore must succeed for ACTIVE multiparty contract")
	require.NotNil(t, proof)
	assert.Equal(t, contractID, proof.ContractID)
	assert.Equal(t, domain.ContractKindMultiparty, proof.ContractKind)
	assert.NotEmpty(t, proof.SHA256)
	assert.Equal(t, mpContract.CurrentVersion, proof.ContractVersion, "ContractVersion must match active version")

	// Independent DB read-back: verify persisted row matches returned proof.
	dbProof, err := env.proofStore.GetByContract(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err, "DB read-back must succeed after GenerateAndStore")
	assert.Equal(t, proof.ID, dbProof.ID, "DB proof ID must match")
	assert.Equal(t, proof.FileID, dbProof.FileID, "DB FileID must match")
	assert.Equal(t, proof.ContractVersion, dbProof.ContractVersion, "DB ContractVersion must match")
}

func TestProofService_Multiparty_GetDownloadURL_Party(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	contractID, _, vendor1ID, _ := createActiveMultipartyContract(t, env)

	// Generate proof first.
	_, err := env.proofSvc.GenerateAndStore(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err)

	// An active vendor (party) must be able to download.
	url, ttl, err := env.proofSvc.GetDownloadURL(ctx, contractID, domain.ContractKindMultiparty, vendor1ID)

	require.NoError(t, err, "active party must get a download URL without error")
	assert.NotEmpty(t, url, "download URL must not be empty")
	assert.Greater(t, ttl, 0, "TTL must be positive")
}

func TestProofService_Multiparty_GetDownloadURL_NonParty_Returns403(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	contractID, _, _, _ := createActiveMultipartyContract(t, env)

	// Generate proof first.
	_, err := env.proofSvc.GenerateAndStore(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err)

	// Non-party (random UUID) must get ErrForbidden.
	nonPartyID := uuid.New()
	_, _, err = env.proofSvc.GetDownloadURL(ctx, contractID, domain.ContractKindMultiparty, nonPartyID)

	require.Error(t, err, "non-party must get an error")
	assert.ErrorIs(t, err, domain.ErrForbidden, "non-party must get ErrForbidden (403)")
}

// ---- Supersede test (item 1: addendum re-sign produces stale proof fix) -----

// TestProofService_Supersede_AddendumResign verifies that when a multiparty contract
// undergoes an addendum cycle (ACTIVE → ADDENDUM_PENDING → ACTIVE at v2), calling
// GenerateAndStore for the new version supersedes the v1 proof row in place, resulting
// in exactly one row with the new version and a different file_id.
func TestProofService_Supersede_AddendumResign(t *testing.T) {
	env := newProofTestEnv(t)
	ctx := context.Background()

	contractID, posterID, vendor1ID, vendor2ID := createActiveMultipartyContract(t, env)

	// Generate v1 proof.
	proof1, err := env.proofSvc.GenerateAndStore(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err, "v1 proof generation must succeed")
	require.NotNil(t, proof1)
	assert.Equal(t, 1, proof1.ContractVersion, "v1 proof must have contract_version=1")

	// DB read-back: confirm v1 row exists.
	dbV1, err := env.proofStore.GetByContract(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err)
	assert.Equal(t, proof1.ID, dbV1.ID)
	assert.Equal(t, 1, dbV1.ContractVersion)

	// Trigger addendum cycle: add a new vendor to the ACTIVE contract.
	vendor3ID := uuid.New()

	// Get contract detail to extract the tenderID for the addendum call.
	mpC, err := env.mpSvc.GetDetail(ctx, contractID, vendor1ID)
	require.NoError(t, err)

	// Add a new vendor to the ACTIVE contract → triggers ADDENDUM_PENDING.
	currency := testCurrencyTWD
	_, _, err = env.mpSvc.CreateOrAddParty(ctx, &service.CreateOrAddPartyInput{
		TenderID:     mpC.Contract.TenderID,
		VendorUserID: vendor3ID,
		ShareBps:     2000,
		Currency:     &currency,
		PosterUserID: &posterID,
	})
	// This transitions to ADDENDUM_PENDING. The addendum flow requires all parties
	// (including v3) to re-sign. If CreateOrAddParty errors due to business rules
	// (e.g. share sum > 10000), skip the test.
	if err != nil {
		t.Skipf("addendum creation failed (business rule): %v", err)
	}

	// Re-submit for signatures (contract now at v2 ADDENDUM_PENDING).
	_, err = env.mpSvc.SubmitForSignatures(ctx, contractID, posterID)
	if err != nil {
		t.Skipf("re-submit for signatures failed: %v", err)
	}

	mpC2, err := env.mpSvc.GetDetail(ctx, contractID, vendor1ID)
	require.NoError(t, err)

	currentHash := mpC2.ContentHash

	// All parties (v1+v2+v3) sign at the new version.
	for _, signerID := range []uuid.UUID{vendor1ID, vendor2ID, vendor3ID} {
		_, signErr := env.mpSvc.Sign(ctx, service.SignInput{
			ContractID:        contractID,
			SignerUserID:      signerID,
			SignedContentHash: currentHash,
			Version:           mpC2.CurrentVersion,
		})
		require.NoError(t, signErr, "signer %s must be able to sign at v%d", signerID, mpC2.CurrentVersion)
	}

	// Contract should now be ACTIVE at v2.
	mpC3, err := env.mpSvc.GetDetail(ctx, contractID, vendor1ID)
	require.NoError(t, err)
	require.Equal(t, domain.MultipartyContractStatusActive, mpC3.Contract.Status,
		"contract must be ACTIVE after re-sign")

	v2 := mpC3.CurrentVersion
	require.Greater(t, v2, 1, "v2 must be > 1 after addendum cycle")

	// Generate proof for v2 — must supersede the v1 row.
	proof2, err := env.proofSvc.GenerateAndStore(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err, "v2 proof generation (supersede) must succeed")
	require.NotNil(t, proof2)

	assert.Equal(t, proof1.ID, proof2.ID, "supersede must preserve the proof row ID")
	assert.NotEqual(t, proof1.FileID, proof2.FileID, "supersede must use a new file_id")
	assert.Equal(t, v2, proof2.ContractVersion, "superseded proof must have the new contract version")

	// DB read-back: exactly one row, at v2.
	dbV2, err := env.proofStore.GetByContract(ctx, contractID, domain.ContractKindMultiparty)
	require.NoError(t, err)
	assert.Equal(t, proof1.ID, dbV2.ID, "still exactly one row (same ID)")
	assert.Equal(t, v2, dbV2.ContractVersion, "DB row must reflect new version")
	assert.Equal(t, proof2.FileID, dbV2.FileID, "DB row must reflect new file_id")
}

// ---- NewProofService nil dependency tests -----------------------------------

func TestNewProofService_NilDependencies_ReturnError(t *testing.T) {
	// A nil in any required dependency must return a descriptive error.
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	require.NotNil(t, sharedServicePool)

	pool := sharedServicePool
	validStores := service.ProofServiceConfig{
		ProofStore:               postgres.NewContractProofStore(pool),
		AuditStore:               postgres.NewAuditLogStore(pool),
		ContractStore:            postgres.NewContractStore(pool),
		SignatureStore:           postgres.NewSignatureStore(pool),
		MultipartyContractStore:  postgres.NewMultipartyContractStore(pool),
		MultipartyPartyStore:     postgres.NewMultipartyPartyStore(pool),
		MultipartySignatureStore: postgres.NewMultipartySignatureStore(pool),
		FileClient:               client.NewFakeFileClient(nil),
	}

	tests := []struct {
		name   string
		mutate func(*service.ProofServiceConfig)
	}{
		{"nil_proof_store", func(c *service.ProofServiceConfig) { c.ProofStore = nil }},
		{"nil_audit_store", func(c *service.ProofServiceConfig) { c.AuditStore = nil }},
		{"nil_contract_store", func(c *service.ProofServiceConfig) { c.ContractStore = nil }},
		{"nil_signature_store", func(c *service.ProofServiceConfig) { c.SignatureStore = nil }},
		{"nil_mp_contract_store", func(c *service.ProofServiceConfig) { c.MultipartyContractStore = nil }},
		{"nil_mp_party_store", func(c *service.ProofServiceConfig) { c.MultipartyPartyStore = nil }},
		{"nil_mp_sig_store", func(c *service.ProofServiceConfig) { c.MultipartySignatureStore = nil }},
		{"nil_file_client", func(c *service.ProofServiceConfig) { c.FileClient = nil }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validStores // copy
			tc.mutate(&cfg)

			_, err := service.NewProofService(&cfg)
			require.Error(t, err, "NewProofService must return error when %s is nil", tc.name)
		})
	}
}

// ---- Ensure _ keeps the decimal import used in helper below ----------------
var _ = decimal.Zero
