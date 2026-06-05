package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// InternalPartiesHandler handles the S2S roster endpoint.
// Mounted at GET /internal/v1/contracts/:id/parties.
// Protected by RequireServiceToken middleware — NOT reachable from the public group.
//
// Returns the frozen ACTIVE-party roster for a multiparty contract:
//
//	[{"vendorUserId":"...", "shareBps": 3000}, ...]
//
// This is what payment calls at settlement-plan creation to obtain the
// authoritative share allocation for disbursement.
type InternalPartiesHandler struct {
	svc *service.MilestoneService
}

// NewInternalPartiesHandler returns an InternalPartiesHandler wired through MilestoneService.
func NewInternalPartiesHandler(svc *service.MilestoneService) *InternalPartiesHandler {
	return &InternalPartiesHandler{svc: svc}
}

// partyRosterEntry is the response shape for one party in the roster.
// Matches the shape payment P3 PR2 will consume.
type partyRosterEntry struct {
	VendorUserID uuid.UUID `json:"vendorUserId"`
	ShareBps     int       `json:"shareBps"`
}

// GetParties handles GET /internal/v1/contracts/:id/parties.
// Returns the frozen ACTIVE-party roster [{vendorUserId, shareBps}].
// Returns 404 if the contract does not exist — a phantom/unknown contract must
// never return a successful empty roster that payment would use to build a
// settlement plan.
func (h *InternalPartiesHandler) GetParties(c *gin.Context) {
	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	parties, err := h.svc.GetPartyRoster(c.Request.Context(), contractID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	roster := make([]partyRosterEntry, len(parties))
	for i, p := range parties {
		roster[i] = partyRosterEntry{
			VendorUserID: p.VendorUserID,
			ShareBps:     p.ShareBps,
		}
	}

	httpx.OK(c, roster)
}
