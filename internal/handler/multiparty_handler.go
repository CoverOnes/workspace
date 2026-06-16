package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// MultipartyHandler handles multi-party contract endpoints.
// Read endpoints (GET) are authenticated via RequireValidIdentity;
// the create-or-add-party endpoint is S2S only (RequireServiceToken).
type MultipartyHandler struct {
	svc      *service.MultipartyContractService
	proofSvc *service.ProofService // optional; nil = proof endpoints return 404
}

// NewMultipartyHandler returns a MultipartyHandler.
func NewMultipartyHandler(svc *service.MultipartyContractService) *MultipartyHandler {
	return &MultipartyHandler{svc: svc}
}

// NewMultipartyHandlerWithProof returns a MultipartyHandler with proof download support.
//
//   - svc: the multiparty contract service.
//   - proofSvc: the proof service (may be nil if file service is not configured).
func NewMultipartyHandlerWithProof(svc *service.MultipartyContractService, proofSvc *service.ProofService) *MultipartyHandler {
	return &MultipartyHandler{svc: svc, proofSvc: proofSvc}
}

// CreateOrAddPartyRequest is the POST /internal/v1/multiparty-contracts request body.
// All fields are marketplace-authoritative; this endpoint is S2S only, not browser-facing.
type CreateOrAddPartyRequest struct {
	TenderID     string  `json:"tenderId"`
	VendorUserID string  `json:"vendorUserId"`
	RoleID       *string `json:"roleId,omitempty"`
	ShareBps     int     `json:"shareBps"`
	Currency     *string `json:"currency,omitempty"`
	// PosterUserID is the tender owner; stored on first contract creation so that
	// milestone management can be gated to the poster only. Optional for backward
	// compatibility with existing marketplace callers.
	PosterUserID *string `json:"posterUserId,omitempty"`
}

// CreateOrAddParty handles POST /internal/v1/multiparty-contracts.
// Idempotent: creates the contract if none exists for tender_id, then adds the party.
// Called by marketplace when an approved collaborator is APPROVED for a tender.
// Protected by RequireServiceToken middleware (S2S only).
//
// S2S trust model: X-Service-Token is the trust boundary for this group.
// The posterUserId field is caller-asserted and is ONLY trusted at first creation (when no
// contract exists for the tender yet). On existing contracts the service validates the
// caller-supplied posterUserId against the stored contract.PosterUserID — a mismatch is
// rejected as ErrForbidden. posterUserId cannot be changed after the contract is created.
func (h *MultipartyHandler) CreateOrAddParty(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	var req CreateOrAddPartyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	tenderID, err := uuid.Parse(req.TenderID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid tenderId")
		return
	}

	vendorUserID, err := uuid.Parse(req.VendorUserID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid vendorUserId")
		return
	}

	var roleID *uuid.UUID

	if req.RoleID != nil {
		parsed, parseErr := uuid.Parse(*req.RoleID)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid roleId")
			return
		}

		roleID = &parsed
	}

	var posterUserID *uuid.UUID

	if req.PosterUserID != nil {
		parsed, parseErr := uuid.Parse(*req.PosterUserID)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid posterUserId")
			return
		}

		posterUserID = &parsed
	}

	in := &service.CreateOrAddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorUserID,
		RoleID:       roleID,
		ShareBps:     req.ShareBps,
		Currency:     req.Currency,
		PosterUserID: posterUserID,
	}

	contract, party, err := h.svc.CreateOrAddParty(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, gin.H{
		"contract": contract,
		"party":    party,
	})
}

// SubmitForSignatures handles POST /v1/multiparty-contracts/:id/submit-for-signature.
// Transitions DRAFT → PENDING_SIGNATURES after validating Σ(share_bps) == 10000.
// Owner-only: caller must be the PosterUserID of the contract.
func (h *MultipartyHandler) SubmitForSignatures(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	contract, err := h.svc.SubmitForSignatures(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// MultipartySignRequest is the POST /v1/multiparty-contracts/:id/sign request body.
type MultipartySignRequest struct {
	SignedContentHash string `json:"signedContentHash"`
	Version           int    `json:"version"`
}

// Sign handles POST /v1/multiparty-contracts/:id/sign.
// A party submits their signature for the current contract version.
func (h *MultipartyHandler) Sign(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	var req MultipartySignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	if req.SignedContentHash == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "signedContentHash is required")
		return
	}

	if req.Version <= 0 {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "version must be >= 1")
		return
	}

	// Signer identity is extracted from the RequireValidIdentity middleware context.
	// Only vendors registered as ACTIVE parties can sign — the service / store enforce
	// this via the unique index and GetActivePartyByVendor check.
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	in := service.SignInput{
		ContractID:        id,
		SignerUserID:      identity.UserID,
		SignedContentHash: req.SignedContentHash,
		Version:           req.Version,
	}

	contract, err := h.svc.Sign(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// UpdatePartyShareRequest is the PATCH /v1/multiparty-contracts/:id/parties/:partyId/share body.
type UpdatePartyShareRequest struct {
	ShareBps int `json:"shareBps"`
}

// UpdatePartyShare handles PATCH /v1/multiparty-contracts/:id/parties/:partyId/share.
// Updates a party's share_bps on an ADDENDUM_PENDING contract.
// Owner-only: caller must be the PosterUserID of the contract.
func (h *MultipartyHandler) UpdatePartyShare(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	partyID, err := uuid.Parse(c.Param("partyId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid partyId")
		return
	}

	var req UpdatePartyShareRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	updated, svcErr := h.svc.UpdatePartyShare(c.Request.Context(), service.UpdatePartyShareInput{
		ContractID:   contractID,
		PartyID:      partyID,
		CallerUserID: identity.UserID,
		NewShareBps:  req.ShareBps,
	})
	if svcErr != nil {
		httpx.Err(c, svcErr)
		return
	}

	httpx.OK(c, updated)
}

// GetProof handles GET /v1/multiparty-contracts/:id/proof.
// Returns a single-use presigned download URL for the signed-contract proof PDF.
// Only ACTIVE parties of the contract may download; non-parties receive 403.
func (h *MultipartyHandler) GetProof(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	if h.proofSvc == nil {
		httpx.ErrCode(c, http.StatusNotFound, "NOT_FOUND", "proof service not available")
		return
	}

	downloadURL, ttl, err := h.proofSvc.GetDownloadURL(c.Request.Context(), id, domain.ContractKindMultiparty, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, gin.H{"url": downloadURL, "ttlSeconds": ttl})
}

// GetDetail handles GET /v1/multiparty-contracts/:id.
// Returns contract + roster + per-version signature progress.
// Access is scoped to ACTIVE parties of the contract (non-party → 404).
func (h *MultipartyHandler) GetDetail(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	detail, err := h.svc.GetDetail(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, detail)
}
