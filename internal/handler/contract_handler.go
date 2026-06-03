package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// NOTE (M-2 fix): The public POST /v1/contracts endpoint and its handler method
// Create() have been removed. Contract creation is now exclusively triggered by
// the marketplace service via POST /internal/v1/contracts after AcceptBid succeeds.
// Clients can no longer supply freelancerUserId, amount, currency, listingId, or
// acceptedBidId — these values come from the authoritative marketplace award record.
// See InternalContractHandler.Create in internal_contract_handler.go.

const maxBodyBytes = 1 << 20 // 1 MB

// ContractHandler handles contract CRUD and lifecycle endpoints.
type ContractHandler struct {
	svc *service.ContractService
}

// NewContractHandler returns a ContractHandler.
func NewContractHandler(svc *service.ContractService) *ContractHandler {
	return &ContractHandler{svc: svc}
}

// List handles GET /v1/contracts.
func (h *ContractHandler) List(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	filter := store.ContractFilter{
		PartyUserID: identity.UserID,
		Limit:       20,
	}

	if statusStr := c.Query("status"); statusStr != "" {
		s := domain.ContractStatus(statusStr)

		switch s {
		case domain.ContractStatusDraft, domain.ContractStatusPendingSignature,
			domain.ContractStatusSigned, domain.ContractStatusActive,
			domain.ContractStatusCompleted, domain.ContractStatusCanceled:
			filter.Status = &s
		default:
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid status filter")
			return
		}
	}

	contracts, err := h.svc.ListContracts(c.Request.Context(), filter)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if contracts == nil {
		contracts = []*domain.Contract{}
	}

	httpx.OK(c, contracts)
}

// GetByID handles GET /v1/contracts/:id.
func (h *ContractHandler) GetByID(c *gin.Context) {
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

	contract, err := h.svc.GetContract(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// PatchContractRequest is the PATCH /v1/contracts/:id request body.
type PatchContractRequest struct {
	Title    *string `json:"title"`
	Terms    *string `json:"terms"`
	Amount   *string `json:"amount"`
	Currency *string `json:"currency"`
}

// Patch handles PATCH /v1/contracts/:id.
func (h *ContractHandler) Patch(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

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

	var req PatchContractRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := service.PatchContractInput{
		ID:       id,
		CallerID: identity.UserID,
		Title:    req.Title,
		Terms:    req.Terms,
		Currency: req.Currency,
	}

	if req.Amount != nil {
		d, parseErr := decimal.NewFromString(*req.Amount)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "amount must be a valid decimal")
			return
		}

		in.Amount = &d
	}

	contract, err := h.svc.PatchContract(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// Submit handles POST /v1/contracts/:id/submit.
func (h *ContractHandler) Submit(c *gin.Context) {
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

	contract, err := h.svc.SubmitContract(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// Complete handles POST /v1/contracts/:id/complete.
func (h *ContractHandler) Complete(c *gin.Context) {
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

	contract, err := h.svc.CompleteContract(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// Cancel handles POST /v1/contracts/:id/cancel.
func (h *ContractHandler) Cancel(c *gin.Context) {
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

	contract, err := h.svc.CancelContract(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}
