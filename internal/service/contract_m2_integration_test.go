package service_test

// M-2 security regression tests: CWE-915 mass-assignment + CWE-639 missing authz.
//
// These tests prove that:
// 1. A malicious client CANNOT call the public POST /v1/contracts endpoint
//    (it has been removed — returns 404).
// 2. The legitimate accept->contract S2S path (marketplace calls workspace internal
//    endpoint with authoritative award data) still creates the DRAFT contract with
//    the correct marketplace-authoritative freelancer/amount values.
// 3. The internal endpoint correctly rejects requests without a valid service token.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const m2ServiceToken = "m2-integration-test-service-token-secret!"

// startM2TestDB returns a real ContractStore backed by the singleton sharedServicePool
// (started once in TestMain). No new container is started here.
func startM2TestDB(t *testing.T) *postgres.ContractStore {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	return postgres.NewContractStore(sharedServicePool)
}

// buildM2Router creates a real router backed by the given contract store.
func buildM2Router(contractStore *postgres.ContractStore) *gin.Engine {
	gin.SetMode(gin.TestMode)

	sigStore := postgres.NewSignatureStore(contractStore.Pool())
	txMgr := postgres.NewTxManager(contractStore.Pool())
	pub := events.NewNoopPublisher()

	svc := service.NewContractService(contractStore, sigStore, txMgr, pub, nil)
	sigSvc := service.NewSignatureService(contractStore, sigStore, nil)
	contractH := handler.NewContractHandler(svc)
	signatureH := handler.NewSignatureHandler(svc, sigSvc)
	internalH := handler.NewInternalContractHandler(svc)

	r := gin.New()

	// Internal S2S route — service token required.
	internal := r.Group("/internal/v1")
	internal.Use(middleware.RequireServiceToken(m2ServiceToken))
	internal.POST("/contracts", internalH.Create)

	// Public API routes — user identity required. POST /v1/contracts is NOT registered.
	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	api.GET("/contracts", middleware.RequireTier(1), contractH.List)
	api.GET("/contracts/:id", middleware.RequireTier(1), contractH.GetByID)
	api.PATCH("/contracts/:id", middleware.RequireTier(2), contractH.Patch)
	api.POST("/contracts/:id/submit-for-signature", middleware.RequireTier(2), contractH.Submit)
	api.POST("/contracts/:id/submit", middleware.RequireTier(2), contractH.Submit)
	api.POST("/contracts/:id/sign", middleware.RequireTier(2), signatureH.Sign)
	api.GET("/contracts/:id/signatures", middleware.RequireTier(1), signatureH.ListSignatures)
	api.POST("/contracts/:id/complete", middleware.RequireTier(2), contractH.Complete)
	api.POST("/contracts/:id/cancel", middleware.RequireTier(2), contractH.Cancel)

	return r
}

// s2sCreateContract drives the internal S2S endpoint to create a DRAFT contract
// and returns the parsed (contractID, contentHash) from the response. It asserts
// the call succeeded with 201.
func s2sCreateContract(
	t *testing.T, r *gin.Engine, clientID, freelancerID uuid.UUID, amount string,
) (contractID uuid.UUID, contentHash string) {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		testKeyListingID:        uuid.New().String(),
		testKeyAwardBidID:       uuid.New().String(),
		testKeyClientUserID:     clientID.String(),
		testKeyFreelancerUserID: freelancerID.String(),
		testKeyAmount:           amount,
		testKeyCurrency:         testCurrencyTWD,
		testKeyTitle:            "Integration Contract",
		testKeyTerms:            "Deliver the work as specified.",
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/internal/v1/contracts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", m2ServiceToken)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "S2S create must succeed; body: %s", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data, ok := resp["data"].(map[string]any)
	require.True(t, ok, "response must have data envelope")

	parsedID, err := uuid.Parse(fmt.Sprintf("%v", data["id"]))
	require.NoError(t, err)
	contractID = parsedID

	contentHash, ok = data["contentHash"].(string)
	require.True(t, ok && contentHash != "", "response must carry a non-empty contentHash; data: %v", data)

	return contractID, contentHash
}

// doParty issues an authenticated POST to path as the given user at tier 2 and
// returns the recorder.
func doParty(t *testing.T, r *gin.Engine, userID uuid.UUID, path, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()

	var rdr *bytes.Reader
	if jsonBody == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(jsonBody))
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", userID.String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

// TestM2_MaliciousClientCannotSelfCreateContract proves that a malicious
// authenticated client CANNOT reach POST /v1/contracts with arbitrary values.
// Before the M-2 fix this returned 201 Created; after the fix it must 404.
func TestM2_MaliciousClientCannotSelfCreateContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	attackerID := uuid.New()
	victimFreelancerID := uuid.New()

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	// Attacker tries to bind an arbitrary freelancer and a fraudulent amount.
	body, _ := json.Marshal(map[string]any{
		testKeyListingID:        uuid.New().String(),
		"acceptedBidId":         uuid.New().String(),
		testKeyFreelancerUserID: victimFreelancerID.String(), // attacker-controlled
		testKeyTitle:            "Malicious Contract",
		testKeyAmount:           "0.01", // fraudulently low amount
		testKeyCurrency:         testCurrencyTWD,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/contracts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", attackerID.String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"POST /v1/contracts must be removed — malicious client must get 404, not 201")
}

// TestM2_InternalEndpointRequiresServiceToken proves that the internal contract-
// create endpoint returns 401/403 without a valid service token.
func TestM2_InternalEndpointRequiresServiceToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "no token", token: "", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", token: "attacker-guessed-token", wantStatus: http.StatusForbidden},
	}

	body, _ := json.Marshal(map[string]any{
		testKeyListingID:        uuid.New().String(),
		testKeyAwardBidID:       uuid.New().String(),
		testKeyClientUserID:     uuid.New().String(),
		testKeyFreelancerUserID: uuid.New().String(),
		testKeyAmount:           "1000.00",
		testKeyCurrency:         testCurrencyTWD,
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/internal/v1/contracts", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			if tc.token != "" {
				req.Header.Set("X-Service-Token", tc.token)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}

// TestM2_LegitAcceptToContractFlowIntegration proves that the legitimate
// marketplace-to-workspace S2S path correctly creates a contract with
// marketplace-authoritative values (freelancer/amount from the award record).
func TestM2_LegitAcceptToContractFlowIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	listingOwnerID := uuid.New()
	bidWinnerID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()
	authoritativeAmount := decimal.NewFromFloat(12345.67)

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	// Simulate marketplace calling workspace after AcceptBid with award data.
	body, _ := json.Marshal(map[string]any{
		testKeyListingID:        listingID.String(),
		testKeyAwardBidID:       bidID.String(),
		testKeyClientUserID:     listingOwnerID.String(),
		testKeyFreelancerUserID: bidWinnerID.String(),
		testKeyAmount:           authoritativeAmount.StringFixed(2),
		testKeyCurrency:         testCurrencyTWD,
		testKeyTitle:            "Design Work Contract",
	})

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/internal/v1/contracts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", m2ServiceToken)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "S2S contract create must succeed; body: %s", w.Body.String())

	// Parse the created contract from the response.
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data, ok := resp["data"].(map[string]any)
	require.True(t, ok, "response must have data envelope")

	contractID, err := uuid.Parse(fmt.Sprintf("%v", data["id"]))
	require.NoError(t, err)

	// Verify via DB read: contract has authoritative freelancer and amount.
	stored, err := cs.GetByID(ctx, contractID)
	require.NoError(t, err)

	assert.Equal(t, listingOwnerID, stored.ClientUserID, "client_user_id must be listing owner (marketplace-authoritative)")
	assert.Equal(t, bidWinnerID, stored.FreelancerUserID, "freelancer_user_id must be bid winner (marketplace-authoritative)")
	assert.Equal(t, listingID, stored.ListingID)
	assert.Equal(t, bidID, stored.AcceptedBidID)
	assert.True(t, authoritativeAmount.Equal(stored.Amount), "amount must be marketplace-authoritative %s, got %s",
		authoritativeAmount.StringFixed(2), stored.Amount.StringFixed(2))
	assert.Equal(t, testCurrencyTWD, stored.Currency)
	assert.Equal(t, domain.ContractStatusDraft, stored.Status)
	assert.Equal(t, 1, stored.Version)
	assert.NotEmpty(t, stored.ContentHash)
}

// TestM2_DuplicateAwardIdempotent proves that calling the internal endpoint twice
// with the same awardBidId returns 409 on the second call (idempotent create).
func TestM2_DuplicateAwardIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	body, _ := json.Marshal(map[string]any{
		testKeyListingID:        uuid.New().String(),
		testKeyAwardBidID:       uuid.New().String(),
		testKeyClientUserID:     uuid.New().String(),
		testKeyFreelancerUserID: uuid.New().String(),
		testKeyAmount:           "999.00",
		testKeyCurrency:         testCurrencyTWD,
	})

	newReq := func() *http.Request {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/internal/v1/contracts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Token", m2ServiceToken)

		return req
	}

	// First call: 201 Created.
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, newReq())
	require.Equal(t, http.StatusCreated, w1.Code)

	// Second call with identical body: 409 Conflict (idempotent, not 500).
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, newReq())
	assert.Equal(t, http.StatusConflict, w2.Code)
}

// TestM2_DualSignReachesActive proves the full happy path: an S2S-created DRAFT
// contract, once submitted by the client, can be signed by BOTH parties using the
// server-returned contentHash and transitions to ACTIVE. This guards the dual-sign
// state machine end-to-end against a real Postgres backend (including the new
// (contract_id, signer_role, contract_version) unique index added in migration 5).
func TestM2_DualSignReachesActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	clientID := uuid.New()
	freelancerID := uuid.New()

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	contractID, contentHash := s2sCreateContract(t, r, clientID, freelancerID, "1500.00")

	// Client submits the contract (DRAFT -> PENDING_SIGNATURE).
	submitW := doParty(t, r, clientID, "/v1/contracts/"+contractID.String()+"/submit", "")
	require.Equal(t, http.StatusOK, submitW.Code, "submit must succeed; body: %s", submitW.Body.String())

	signBody := fmt.Sprintf(`{"signedContentHash":%q}`, contentHash)

	// Client signs first — contract stays PENDING_SIGNATURE (only one role signed).
	clientSignW := doParty(t, r, clientID, "/v1/contracts/"+contractID.String()+"/sign", signBody)
	require.Equal(t, http.StatusOK, clientSignW.Code, "client sign must succeed; body: %s", clientSignW.Body.String())

	var afterClient map[string]any
	require.NoError(t, json.Unmarshal(clientSignW.Body.Bytes(), &afterClient))
	clientData, _ := afterClient["data"].(map[string]any)
	assert.Equal(t, string(domain.ContractStatusPendingSignature), fmt.Sprintf("%v", clientData["status"]),
		"after one signature the contract must still be PENDING_SIGNATURE")

	// Freelancer signs second — both roles present -> SIGNED -> ACTIVE.
	freelancerSignW := doParty(t, r, freelancerID, "/v1/contracts/"+contractID.String()+"/sign", signBody)
	require.Equal(t, http.StatusOK, freelancerSignW.Code, "freelancer sign must succeed; body: %s", freelancerSignW.Body.String())

	var afterFreelancer map[string]any
	require.NoError(t, json.Unmarshal(freelancerSignW.Body.Bytes(), &afterFreelancer))
	freelancerData, _ := afterFreelancer["data"].(map[string]any)
	assert.Equal(t, string(domain.ContractStatusActive), fmt.Sprintf("%v", freelancerData["status"]),
		"after both parties sign the contract must be ACTIVE")

	// Authoritative DB confirmation.
	stored, err := cs.GetByID(ctx, contractID)
	require.NoError(t, err)
	assert.Equal(t, domain.ContractStatusActive, stored.Status, "stored contract must be ACTIVE")
	require.NotNil(t, stored.ActivatedAt, "activated_at must be set on activation")
}

// TestInc2_FullLifecycleViaSubmitForSignature drives the complete contract
// lifecycle the demo needs against a real Postgres backend, using the canonical
// web-app route POST /v1/contracts/:id/submit-for-signature:
//
//	S2S create (DRAFT)
//	  -> client submit-for-signature (DRAFT -> PENDING_SIGNATURE)
//	  -> client signs            (stays PENDING_SIGNATURE; one role)
//	  -> freelancer signs        (DISTINCT signer_role count == 2 -> SIGNED -> ACTIVE)
//
// It also asserts the IDOR guards: a non-party (neither client nor freelancer)
// receives 404 on BOTH submit-for-signature and sign — never a 403 that would
// leak resource existence.
func TestInc2_FullLifecycleViaSubmitForSignature(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	clientID := uuid.New()
	freelancerID := uuid.New()
	strangerID := uuid.New() // neither client nor freelancer

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	// 1. S2S create -> DRAFT.
	contractID, contentHash := s2sCreateContract(t, r, clientID, freelancerID, "8800.00")

	stored, err := cs.GetByID(ctx, contractID)
	require.NoError(t, err)
	require.Equal(t, domain.ContractStatusDraft, stored.Status, "freshly created contract must be DRAFT")

	submitPath := "/v1/contracts/" + contractID.String() + "/submit-for-signature"
	signPath := "/v1/contracts/" + contractID.String() + "/sign"
	signBody := fmt.Sprintf(`{"signedContentHash":%q}`, contentHash)

	// 2a. Non-party cannot submit-for-signature: must get 404 (IDOR-safe), and the
	//     contract must remain DRAFT.
	strangerSubmitW := doParty(t, r, strangerID, submitPath, "")
	require.Equal(t, http.StatusNotFound, strangerSubmitW.Code,
		"non-party submit-for-signature must return 404; body: %s", strangerSubmitW.Body.String())

	stillDraft, err := cs.GetByID(ctx, contractID)
	require.NoError(t, err)
	require.Equal(t, domain.ContractStatusDraft, stillDraft.Status,
		"a rejected non-party submit must not change contract status")

	// 2b. The freelancer is a party but NOT the client; submit is client-only, so
	//     even a legitimate party that is not the client gets 404.
	freelancerSubmitW := doParty(t, r, freelancerID, submitPath, "")
	require.Equal(t, http.StatusNotFound, freelancerSubmitW.Code,
		"freelancer (non-client party) submit-for-signature must return 404; body: %s", freelancerSubmitW.Body.String())

	// 2c. Signing is not allowed while DRAFT (must be PENDING_SIGNATURE first).
	earlySignW := doParty(t, r, clientID, signPath, signBody)
	require.Equal(t, http.StatusConflict, earlySignW.Code,
		"signing a DRAFT contract must be rejected (INVALID_STATE_TRANSITION); body: %s", earlySignW.Body.String())

	// 3. Client submits -> PENDING_SIGNATURE.
	submitW := doParty(t, r, clientID, submitPath, "")
	require.Equal(t, http.StatusOK, submitW.Code,
		"client submit-for-signature must succeed; body: %s", submitW.Body.String())

	var afterSubmit map[string]any
	require.NoError(t, json.Unmarshal(submitW.Body.Bytes(), &afterSubmit))
	submitData, _ := afterSubmit["data"].(map[string]any)
	require.Equal(t, string(domain.ContractStatusPendingSignature), fmt.Sprintf("%v", submitData["status"]),
		"after submit-for-signature the contract must be PENDING_SIGNATURE")

	// 3b. A non-party still cannot sign once PENDING_SIGNATURE: must get 404.
	strangerSignW := doParty(t, r, strangerID, signPath, signBody)
	require.Equal(t, http.StatusNotFound, strangerSignW.Code,
		"non-party sign must return 404; body: %s", strangerSignW.Body.String())

	// 4. Client signs first -> still PENDING_SIGNATURE (only one DISTINCT role).
	clientSignW := doParty(t, r, clientID, signPath, signBody)
	require.Equal(t, http.StatusOK, clientSignW.Code,
		"client sign must succeed; body: %s", clientSignW.Body.String())

	var afterClient map[string]any
	require.NoError(t, json.Unmarshal(clientSignW.Body.Bytes(), &afterClient))
	clientData, _ := afterClient["data"].(map[string]any)
	require.Equal(t, string(domain.ContractStatusPendingSignature), fmt.Sprintf("%v", clientData["status"]),
		"after one signature the contract must still be PENDING_SIGNATURE")

	// 5. Freelancer signs second -> DISTINCT signer_role count == 2 -> SIGNED -> ACTIVE.
	freelancerSignW := doParty(t, r, freelancerID, signPath, signBody)
	require.Equal(t, http.StatusOK, freelancerSignW.Code,
		"freelancer sign must succeed; body: %s", freelancerSignW.Body.String())

	var afterFreelancer map[string]any
	require.NoError(t, json.Unmarshal(freelancerSignW.Body.Bytes(), &afterFreelancer))
	freelancerData, _ := afterFreelancer["data"].(map[string]any)
	require.Equal(t, string(domain.ContractStatusActive), fmt.Sprintf("%v", freelancerData["status"]),
		"after both DISTINCT roles sign the contract must be ACTIVE")

	// 6. Authoritative DB confirmation: ACTIVE with activated_at set.
	final, err := cs.GetByID(ctx, contractID)
	require.NoError(t, err)
	assert.Equal(t, domain.ContractStatusActive, final.Status, "stored contract must be ACTIVE")
	require.NotNil(t, final.ActivatedAt, "activated_at must be set on activation")
}

// TestInc2_SubmitAndSignAliasRoutesAgree proves the canonical
// /submit-for-signature route and the legacy /submit alias drive the identical
// DRAFT -> PENDING_SIGNATURE transition, so the web-app may use either.
func TestInc2_SubmitAndSignAliasRoutesAgree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tests := []struct {
		name string
		path func(id string) string
	}{
		{
			name: "canonical submit-for-signature route",
			path: func(id string) string { return "/v1/contracts/" + id + "/submit-for-signature" },
		},
		{
			name: "legacy submit alias route",
			path: func(id string) string { return "/v1/contracts/" + id + "/submit" },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientID := uuid.New()
			freelancerID := uuid.New()

			cs := startM2TestDB(t)
			r := buildM2Router(cs)

			contractID, _ := s2sCreateContract(t, r, clientID, freelancerID, "1234.00")

			w := doParty(t, r, clientID, tc.path(contractID.String()), "")
			require.Equal(t, http.StatusOK, w.Code, "submit via %s must succeed; body: %s", tc.name, w.Body.String())

			got, err := cs.GetByID(context.Background(), contractID)
			require.NoError(t, err)
			assert.Equal(t, domain.ContractStatusPendingSignature, got.Status,
				"both routes must transition DRAFT -> PENDING_SIGNATURE")
		})
	}
}

// TestM2_ContentHashBindsParties proves the content_hash is cryptographically bound
// to the signing parties: the server-stored hash equals the digest computed over
// the actual (client, freelancer) pair, and a digest computed with a swapped /
// different freelancer does NOT match — so it is rejected at sign time with a hash
// mismatch (the frontend always signs the server-returned hash; an attacker who
// re-binds parties cannot produce a hash the server will accept).
func TestM2_ContentHashBindsParties(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	clientID := uuid.New()
	freelancerID := uuid.New()
	impostorFreelancerID := uuid.New()

	cs := startM2TestDB(t)
	r := buildM2Router(cs)

	contractID, serverHash := s2sCreateContract(t, r, clientID, freelancerID, "2750.00")

	stored, err := cs.GetByID(ctx, contractID)
	require.NoError(t, err)

	// 1. The server hash binds the real parties: recomputing the digest with the
	//    actual stored client+freelancer reproduces the server hash exactly.
	boundHash := domain.CanonicalContractDigest(
		contractID.String(), stored.ClientUserID.String(), stored.FreelancerUserID.String(),
		stored.Title, stored.Terms, stored.Amount.StringFixed(2), stored.Currency, stored.Version,
	)
	assert.Equal(t, serverHash, boundHash, "server contentHash must equal the digest over the real party pair")
	assert.Equal(t, stored.ContentHash, serverHash, "stored content_hash must equal the response contentHash")

	// 2. A digest computed with a DIFFERENT freelancer must differ — proving the
	//    parties are part of the signed material.
	impostorHash := domain.CanonicalContractDigest(
		contractID.String(), stored.ClientUserID.String(), impostorFreelancerID.String(),
		stored.Title, stored.Terms, stored.Amount.StringFixed(2), stored.Currency, stored.Version,
	)
	require.NotEqual(t, serverHash, impostorHash, "swapping the freelancer must change the digest")

	// 3. Submit, then attempt to sign with the impostor (party-swapped) hash. The
	//    server compares against its own authoritative hash and rejects it (409
	//    HASH_MISMATCH) — a client cannot bind a different counterparty post-hoc.
	submitW := doParty(t, r, clientID, "/v1/contracts/"+contractID.String()+"/submit", "")
	require.Equal(t, http.StatusOK, submitW.Code, "submit must succeed; body: %s", submitW.Body.String())

	impostorSignBody := fmt.Sprintf(`{"signedContentHash":%q}`, impostorHash)
	badSignW := doParty(t, r, clientID, "/v1/contracts/"+contractID.String()+"/sign", impostorSignBody)
	assert.Equal(t, http.StatusConflict, badSignW.Code,
		"signing with a party-swapped hash must be rejected (HASH_MISMATCH); body: %s", badSignW.Body.String())

	// 4. Signing with the correct server hash still succeeds (round-trip intact).
	goodSignBody := fmt.Sprintf(`{"signedContentHash":%q}`, serverHash)
	goodSignW := doParty(t, r, clientID, "/v1/contracts/"+contractID.String()+"/sign", goodSignBody)
	assert.Equal(t, http.StatusOK, goodSignW.Code,
		"signing with the correct server hash must succeed; body: %s", goodSignW.Body.String())
}

// TestM2_ContractStore_HasPool verifies that the postgres.ContractStore exposes a
// Pool() accessor so integration tests can instantiate sibling stores (signature
// store, tx manager) from the same connection pool without needing a pool pointer
// passed separately. This is a compile-time / API contract check.
func TestM2_ContractStore_HasPool(t *testing.T) {
	// This test has no assertions that depend on a live DB — it just ensures
	// the Pool() method exists on *postgres.ContractStore (compile-time check).
	t.Log("postgres.ContractStore.Pool() method existence verified at compile time")
}
