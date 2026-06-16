package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// --- stub stores for handler tests ---

type stubContractStoreH struct {
	contracts map[uuid.UUID]*domain.Contract
}

func newStubContractStoreH(contracts ...*domain.Contract) *stubContractStoreH {
	m := &stubContractStoreH{contracts: make(map[uuid.UUID]*domain.Contract)}

	for _, c := range contracts {
		m.contracts[c.ID] = c
	}

	return m
}

func (s *stubContractStoreH) Create(_ context.Context, c *domain.Contract) error {
	if _, exists := s.contracts[c.ID]; exists {
		return domain.ErrConflict
	}

	s.contracts[c.ID] = c

	return nil
}

func (s *stubContractStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Contract, error) {
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}

	return c, nil
}

func (s *stubContractStoreH) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Contract, error) {
	return s.GetByID(ctx, id)
}

func (s *stubContractStoreH) ListByParty(_ context.Context, filter store.ContractFilter) ([]*domain.Contract, error) {
	var result []*domain.Contract

	for _, c := range s.contracts {
		if c.ClientUserID == filter.PartyUserID || c.FreelancerUserID == filter.PartyUserID {
			result = append(result, c)
		}
	}

	return result, nil
}

func (s *stubContractStoreH) Update(_ context.Context, c *domain.Contract) error {
	if _, ok := s.contracts[c.ID]; !ok {
		return domain.ErrContractNotFound
	}

	s.contracts[c.ID] = c

	return nil
}

type stubSigStoreH struct {
	sigs map[uuid.UUID]*domain.Signature
}

func (s *stubSigStoreH) Create(_ context.Context, sig *domain.Signature) error {
	if s.sigs == nil {
		s.sigs = make(map[uuid.UUID]*domain.Signature)
	}

	s.sigs[sig.ID] = sig

	return nil
}

func (s *stubSigStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Signature, error) {
	if s.sigs == nil {
		return nil, domain.ErrSignatureNotFound
	}

	sig, ok := s.sigs[id]
	if !ok {
		return nil, domain.ErrSignatureNotFound
	}

	return sig, nil
}

func (s *stubSigStoreH) ListByContract(_ context.Context, _ uuid.UUID) ([]*domain.Signature, error) {
	return nil, nil
}

func (s *stubSigStoreH) CountValidSignatures(_ context.Context, _ uuid.UUID, _ int, _ string) (int, error) {
	return 0, nil
}

func (s *stubSigStoreH) SetFileID(_ context.Context, id, fileID uuid.UUID) error {
	if s.sigs == nil {
		return domain.ErrSignatureNotFound
	}

	sig, ok := s.sigs[id]
	if !ok {
		return domain.ErrSignatureNotFound
	}

	sig.FileID = &fileID

	return nil
}

type stubTxH struct {
	contracts store.ContractStore
	sigs      store.SignatureStore
}

func (m *stubTxH) WithTx(
	ctx context.Context,
	fn func(ctx context.Context, c store.ContractStore, s store.SignatureStore, o store.OutboxStore) error,
) error {
	return fn(ctx, m.contracts, m.sigs, &noopOutboxStoreH{})
}

// noopOutboxStoreH is a test double for store.OutboxStore used in handler tests.
type noopOutboxStoreH struct{}

func (*noopOutboxStoreH) Enqueue(_ context.Context, _ *store.OutboxEnqueueInput) error { return nil }

func (*noopOutboxStoreH) FetchPending(_ context.Context, _ int) ([]*domain.OutboxEntry, error) {
	return nil, nil
}
func (*noopOutboxStoreH) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (*noopOutboxStoreH) RecordFailure(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}

func (*noopOutboxStoreH) DeleteOldPublished(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (*noopOutboxStoreH) CountStalePending(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

const testServiceToken = "test-service-token-at-least-32-chars!!"

// buildContractRouter creates a test router with ContractService wired to stub stores.
// The public POST /v1/contracts is intentionally NOT registered (M-2 fix).
func buildContractRouter(cs *stubContractStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	ss := &stubSigStoreH{}
	tx := &stubTxH{contracts: cs, sigs: ss}
	pub := events.NewNoopPublisher()

	svc := service.NewContractService(cs, ss, tx, pub, nil, false)
	h := handler.NewContractHandler(svc)
	internalH := handler.NewInternalContractHandler(svc)

	r := gin.New()
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())

	// Internal S2S endpoint (M-2 fix): protected by service token, NOT by user identity.
	internal := r.Group("/internal/v1")
	internal.Use(middleware.RequireServiceToken(testServiceToken))
	internal.POST("/contracts", internalH.Create)

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())

	// NOTE: POST /v1/contracts is NOT registered here (removed as part of M-2 fix).
	api.GET("/contracts", middleware.RequireTier(1), h.List)
	api.GET("/contracts/:id", middleware.RequireTier(1), h.GetByID)
	api.PATCH("/contracts/:id", middleware.RequireTier(2), h.Patch)
	api.POST("/contracts/:id/submit-for-signature", middleware.RequireTier(2), h.Submit)
	api.POST("/contracts/:id/submit", middleware.RequireTier(2), h.Submit)
	api.POST("/contracts/:id/complete", middleware.RequireTier(2), h.Complete)
	api.POST("/contracts/:id/cancel", middleware.RequireTier(2), h.Cancel)

	return r
}

func makeHandlerContract(clientID, freelancerID uuid.UUID, status domain.ContractStatus) *domain.Contract {
	now := time.Now().UTC()
	amount := decimal.NewFromInt(5000)
	cid := uuid.New()
	hash := domain.CanonicalContractDigest(
		cid.String(), clientID.String(), freelancerID.String(),
		"Test Contract", "Terms body", amount.StringFixed(2), testCurrencyTWD, 1,
	)

	return &domain.Contract{
		ID:               cid,
		ListingID:        uuid.New(),
		AcceptedBidID:    uuid.New(),
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Title:            "Test Contract",
		Terms:            "Terms body",
		Amount:           amount,
		Currency:         testCurrencyTWD,
		ContentHash:      hash,
		Version:          1,
		Status:           status,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// TestPublicContractCreate_Removed verifies that POST /v1/contracts no longer
// exists — this is the core M-2 vulnerability closure. Any client attempting to
// self-create a contract with arbitrary freelancer/amount values must receive 404.
func TestPublicContractCreate_Removed(t *testing.T) {
	t.Parallel()

	cs := newStubContractStoreH()
	r := buildContractRouter(cs)

	body, _ := json.Marshal(map[string]any{
		testKeyListingID:        uuid.New().String(),
		"acceptedBidId":         uuid.New().String(),
		testKeyFreelancerUserID: uuid.New().String(), // attacker-controlled
		testKeyTitle:            "Malicious Contract",
		testKeyAmount:           "0.01", // attacker-controlled amount
		testKeyCurrency:         testCurrencyTWD,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/contracts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", uuid.New().String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Must be 404 — endpoint was removed. Previously this returned 201 Created,
	// allowing the caller to bind any freelancer/amount (CWE-915 / CWE-639).
	assert.Equal(t, http.StatusNotFound, w.Code, "public contract-create must return 404 after M-2 fix")
}

// TestInternalContractCreate verifies the S2S endpoint correctly creates contracts
// and enforces the service-token gate.
func TestInternalContractCreate(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	freelancerID := uuid.New()

	tests := []struct {
		name       string
		token      string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{
			name:  "valid service token and payload creates contract",
			token: testServiceToken,
			body: map[string]any{
				testKeyListingID:        uuid.New().String(),
				testKeyAwardBidID:       uuid.New().String(),
				testKeyClientUserID:     ownerID.String(),
				testKeyFreelancerUserID: freelancerID.String(),
				testKeyAmount:           "5000.00",
				testKeyCurrency:         testCurrencyTWD,
			},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "missing service token returns 401",
			token:      "",
			body:       map[string]any{testKeyListingID: uuid.New().String()},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:       "wrong service token returns 403",
			token:      "wrong-token",
			body:       map[string]any{testKeyListingID: uuid.New().String()},
			wantStatus: http.StatusForbidden,
			wantCode:   "FORBIDDEN",
		},
		{
			name:  "invalid clientUserId UUID returns 400",
			token: testServiceToken,
			body: map[string]any{
				testKeyListingID:        uuid.New().String(),
				testKeyAwardBidID:       uuid.New().String(),
				testKeyClientUserID:     "not-a-uuid",
				testKeyFreelancerUserID: freelancerID.String(),
				testKeyAmount:           "5000.00",
				testKeyCurrency:         testCurrencyTWD,
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   testErrCodeValidation,
		},
		{
			name:  "non-decimal amount returns 400",
			token: testServiceToken,
			body: map[string]any{
				testKeyListingID:        uuid.New().String(),
				testKeyAwardBidID:       uuid.New().String(),
				testKeyClientUserID:     ownerID.String(),
				testKeyFreelancerUserID: freelancerID.String(),
				testKeyAmount:           "not-a-number",
				testKeyCurrency:         testCurrencyTWD,
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   testErrCodeValidation,
		},
		{
			name:  "client == freelancer returns 400 (same-party contract rejected)",
			token: testServiceToken,
			body: map[string]any{
				testKeyListingID:        uuid.New().String(),
				testKeyAwardBidID:       uuid.New().String(),
				testKeyClientUserID:     ownerID.String(),
				testKeyFreelancerUserID: ownerID.String(), // same person
				testKeyAmount:           "1000.00",
				testKeyCurrency:         testCurrencyTWD,
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   testErrCodeValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cs := newStubContractStoreH()
			r := buildContractRouter(cs)

			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/internal/v1/contracts", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			if tc.token != "" {
				req.Header.Set("X-Service-Token", tc.token)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestContractHandler_GetByID_IDOR(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusDraft)
	cs := newStubContractStoreH(contract)
	r := buildContractRouter(cs)

	tests := []struct {
		name       string
		callerID   uuid.UUID
		contractID string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "client can get own contract",
			callerID:   clientID,
			contractID: contract.ID.String(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "freelancer can get contract",
			callerID:   freelancerID,
			contractID: contract.ID.String(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "non-party gets 404 (IDOR guard)",
			callerID:   thirdPartyID,
			contractID: contract.ID.String(),
			wantStatus: http.StatusNotFound,
			wantCode:   testErrCodeNotFound,
		},
		{
			name:       "invalid id returns 400",
			callerID:   clientID,
			contractID: "not-a-uuid",
			wantStatus: http.StatusBadRequest,
			wantCode:   testErrCodeValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/contracts/"+tc.contractID, nil)
			req.Header.Set("X-User-Id", tc.callerID.String())
			req.Header.Set("X-Kyc-Tier", "1")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestContractHandler_Cancel_InvalidState(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusCompleted)
	cs := newStubContractStoreH(contract)
	r := buildContractRouter(cs)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/contracts/"+contract.ID.String()+"/cancel", nil)
	req.Header.Set("X-User-Id", clientID.String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "INVALID_STATE_TRANSITION", errBody["code"])
}
