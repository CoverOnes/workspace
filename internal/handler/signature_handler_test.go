package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildSignatureRouter(cs *stubContractStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	ss := &stubSigStoreH{}
	tx := &stubTxH{contracts: cs, sigs: ss}
	pub := events.NewNoopPublisher()

	contractSvc := service.NewContractService(cs, ss, tx, pub)
	signatureSvc := service.NewSignatureService(cs, ss)
	signatureH := handler.NewSignatureHandler(contractSvc, signatureSvc)

	r := gin.New()
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())

	api.POST("/contracts/:id/sign", middleware.RequireTier(2), signatureH.Sign)
	api.GET("/contracts/:id/signatures", middleware.RequireTier(1), signatureH.ListSignatures)

	return r
}

func TestSignatureHandler_Sign_HashMismatch(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
	cs := newStubContractStoreH(contract)
	r := buildSignatureRouter(cs)

	// Use a valid-format hash (64-char lowercase hex) that does NOT match the contract's
	// content hash. The handler validates format first, then the service checks equality.
	body, _ := json.Marshal(map[string]any{
		"signedContentHash": "0000000000000000000000000000000000000000000000000000000000000000",
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/contracts/"+contract.ID.String()+"/sign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", clientID.String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "HASH_MISMATCH", errBody["code"])
}

func TestSignatureHandler_Sign_InvalidHashFormat(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
	cs := newStubContractStoreH(contract)
	r := buildSignatureRouter(cs)

	invalidCases := []string{
		"not-a-hex-string", // not hex at all
		"ABCDEF1234567890abcdef1234567890abcdef1234567890abcdef1234567890", // uppercase
		"abc", // too short
		"0000000000000000000000000000000000000000000000000000000000000000x", // 65 chars
	}

	for _, hash := range invalidCases {
		t.Run(hash, func(t *testing.T) {
			t.Parallel()

			body, _ := json.Marshal(map[string]any{"signedContentHash": hash})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/v1/contracts/"+contract.ID.String()+"/sign", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", clientID.String())
			req.Header.Set("X-Kyc-Tier", "2")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestSignatureHandler_Sign_RequiresTier2(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
	cs := newStubContractStoreH(contract)
	r := buildSignatureRouter(cs)

	body, _ := json.Marshal(map[string]any{
		"signedContentHash": contract.ContentHash,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/contracts/"+contract.ID.String()+"/sign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", clientID.String())
	req.Header.Set("X-Kyc-Tier", "1") // Tier 1 — sign requires Tier 2

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "KYC_TIER_REQUIRED", errBody["code"])
}

func TestSignatureHandler_Sign_NonPartyGets404(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
	cs := newStubContractStoreH(contract)
	r := buildSignatureRouter(cs)

	body, _ := json.Marshal(map[string]any{
		"signedContentHash": contract.ContentHash,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/contracts/"+contract.ID.String()+"/sign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", thirdPartyID.String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSignatureHandler_ListSignatures_NonPartyGets404(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusActive)
	cs := newStubContractStoreH(contract)
	r := buildSignatureRouter(cs)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/contracts/"+contract.ID.String()+"/signatures", nil)
	req.Header.Set("X-User-Id", thirdPartyID.String())
	req.Header.Set("X-Kyc-Tier", "1")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
