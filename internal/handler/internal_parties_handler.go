package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/store"
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
	parties store.MultipartyPartyStore
}

// NewInternalPartiesHandler returns an InternalPartiesHandler.
func NewInternalPartiesHandler(parties store.MultipartyPartyStore) *InternalPartiesHandler {
	return &InternalPartiesHandler{parties: parties}
}

// partyRosterEntry is the response shape for one party in the roster.
// Matches the shape payment P3 PR2 will consume.
type partyRosterEntry struct {
	VendorUserID uuid.UUID `json:"vendorUserId"`
	ShareBps     int       `json:"shareBps"`
}

// GetParties handles GET /internal/v1/contracts/:id/parties.
// Returns the frozen ACTIVE-party roster [{vendorUserId, shareBps}].
// The contract_id parameter here refers to the multi_party_contract id.
func (h *InternalPartiesHandler) GetParties(c *gin.Context) {
	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	parties, err := h.parties.ListActiveByContract(c.Request.Context(), contractID)
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
