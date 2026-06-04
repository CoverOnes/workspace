package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// InternalContractHandler handles the server-to-server contract creation endpoint.
// This handler is mounted at /internal/v1/contracts and is protected by a
// pre-shared service token (RequireServiceToken middleware). It is NOT reachable
// from the public API — the gateway must not route /internal/* paths externally.
type InternalContractHandler struct {
	svc *service.ContractService
}

// NewInternalContractHandler returns an InternalContractHandler.
func NewInternalContractHandler(svc *service.ContractService) *InternalContractHandler {
	return &InternalContractHandler{svc: svc}
}

// CreateFromAwardRequest is the POST /internal/v1/contracts request body.
// All deal-identity fields originate from the marketplace award record and
// are authoritative — clients cannot influence them.
type CreateFromAwardRequest struct {
	ListingID        string `json:"listingId"`
	AwardBidID       string `json:"awardBidId"`
	ClientUserID     string `json:"clientUserId"`
	FreelancerUserID string `json:"freelancerUserId"`
	Amount           string `json:"amount"` // numeric as string to preserve precision
	Currency         string `json:"currency"`
	Title            string `json:"title,omitempty"`
	Terms            string `json:"terms,omitempty"`
}

// Create handles POST /internal/v1/contracts.
// Called exclusively by the marketplace service after AcceptBid succeeds.
func (h *InternalContractHandler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	var req CreateFromAwardRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	listingID, err := uuid.Parse(req.ListingID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid listingId")
		return
	}

	awardBidID, err := uuid.Parse(req.AwardBidID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid awardBidId")
		return
	}

	clientUserID, err := uuid.Parse(req.ClientUserID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid clientUserId")
		return
	}

	freelancerUserID, err := uuid.Parse(req.FreelancerUserID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid freelancerUserId")
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

	contract, err := h.svc.CreateContractFromAward(c.Request.Context(), &service.CreateContractFromAwardInput{
		ListingID:        listingID,
		AwardBidID:       awardBidID,
		ClientUserID:     clientUserID,
		FreelancerUserID: freelancerUserID,
		Amount:           amount,
		Currency:         currency,
		Title:            req.Title,
		Terms:            req.Terms,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, contract)
}
