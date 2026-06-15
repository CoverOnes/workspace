package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// InternalMilestonesHandler handles S2S milestone aggregate endpoints.
// Mounted at GET /internal/v1/contracts/:id/milestones/amounts.
// Protected by RequireServiceToken middleware — NOT reachable from the public group.
//
// Returns the sum of ALL milestone amounts for a multiparty contract:
//
//	{"data":{"totalAmount":"1500.00"}}
//
// This is what payment calls at settlement-plan creation to obtain the
// authoritative escrow disbursement cap (cumulative cap = sum of all milestones).
type InternalMilestonesHandler struct {
	svc *service.MilestoneService
}

// NewInternalMilestonesHandler returns an InternalMilestonesHandler.
func NewInternalMilestonesHandler(svc *service.MilestoneService) *InternalMilestonesHandler {
	return &InternalMilestonesHandler{svc: svc}
}

// milestoneAmountsSumResponse is the response body for GetAmountsSum.
// totalAmount is a decimal string to preserve numeric(14,2) precision without float64 loss.
type milestoneAmountsSumResponse struct {
	TotalAmount string `json:"totalAmount"`
}

// GetAmountsSum handles GET /internal/v1/contracts/:id/milestones/amounts.
// Returns {"data":{"totalAmount":"..."}} — the Σ of all milestone amounts for the contract.
// Returns 404 if the contract does not exist (phantom guard).
// Returns 422 if the contract is not in ACTIVE or COMPLETED status.
func (h *InternalMilestonesHandler) GetAmountsSum(c *gin.Context) {
	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	sum, err := h.svc.GetMilestoneAmountsSum(c.Request.Context(), contractID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, milestoneAmountsSumResponse{
		TotalAmount: sum.StringFixed(2),
	})
}
