package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// MilestoneHandler handles milestone management endpoints.
// All endpoints are owner-only: the caller must be the contract's PosterUserID.
// Non-owners receive 404 (IDOR guard — same pattern as ErrNotParty).
type MilestoneHandler struct {
	svc *service.MilestoneService
}

// NewMilestoneHandler returns a MilestoneHandler.
func NewMilestoneHandler(svc *service.MilestoneService) *MilestoneHandler {
	return &MilestoneHandler{svc: svc}
}

// AddMilestoneRequest is the POST /v1/multiparty-contracts/:id/milestones request body.
type AddMilestoneRequest struct {
	Name     string `json:"name"`
	Amount   string `json:"amount"` // numeric as string to preserve precision
	Currency string `json:"currency,omitempty"`
	Sequence int    `json:"sequence,omitempty"`
}

// AddMilestone handles POST /v1/multiparty-contracts/:id/milestones.
// Owner-only (PosterUserID). Returns 201 with the created milestone.
func (h *MilestoneHandler) AddMilestone(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	var req AddMilestoneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "amount must be a valid decimal")
		return
	}

	currency := req.Currency
	if currency == "" {
		currency = "TWD"
	}

	m, err := h.svc.AddMilestone(c.Request.Context(), &service.AddMilestoneInput{
		ContractID: contractID,
		CallerID:   identity.UserID,
		Name:       req.Name,
		Amount:     amount,
		Currency:   currency,
		Sequence:   req.Sequence,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, m)
}

// ListMilestones handles GET /v1/multiparty-contracts/:id/milestones.
// Owner-only (PosterUserID). Returns the ordered milestone list.
func (h *MilestoneHandler) ListMilestones(c *gin.Context) {
	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	ms, err := h.svc.ListMilestones(c.Request.Context(), contractID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, ms)
}

// CompleteMilestone handles POST /v1/multiparty-contracts/:id/milestones/:mid/complete.
// Owner-only (PosterUserID). Sets the milestone COMPLETED and emits a
// workspace.contract_completed event (§14 best-effort publish).
func (h *MilestoneHandler) CompleteMilestone(c *gin.Context) {
	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	milestoneID, err := uuid.Parse(c.Param("mid"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid milestone id")
		return
	}

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	m, err := h.svc.CompleteMilestone(c.Request.Context(), service.CompleteMilestoneInput{
		ContractID:  contractID,
		MilestoneID: milestoneID,
		CallerID:    identity.UserID,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, m)
}
