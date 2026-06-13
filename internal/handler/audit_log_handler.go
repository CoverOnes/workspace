package handler

import (
	"net/http"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AuditLogHandler handles the GET /internal/contracts/{id}/audit endpoint.
// The route is in the /internal/v1 group, which is protected by RequireServiceToken
// middleware (§24.1 gated — gateway-injected identity, S2S only).
type AuditLogHandler struct {
	svc *service.AuditLogService
}

// NewAuditLogHandler returns an AuditLogHandler.
func NewAuditLogHandler(svc *service.AuditLogService) *AuditLogHandler {
	return &AuditLogHandler{svc: svc}
}

// auditLogResponse is the response envelope for GET /internal/contracts/{id}/audit.
type auditLogResponse struct {
	Entries    []*domain.ContractAuditLog `json:"entries"`
	Intact     bool                       `json:"intact"`
	EntryCount int                        `json:"entryCount"`
}

// GetAuditLog handles GET /internal/v1/contracts/:id/audit.
// Returns the full audit log for the contract plus an integrity flag.
// The integrity flag is false if any entry's hash has been tampered with.
func (h *AuditLogHandler) GetAuditLog(c *gin.Context) {
	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	entries, intact, err := h.svc.GetAuditLog(c.Request.Context(), contractID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	// Return an empty slice rather than null when there are no entries.
	if entries == nil {
		entries = []*domain.ContractAuditLog{}
	}

	httpx.OK(c, auditLogResponse{
		Entries:    entries,
		Intact:     intact,
		EntryCount: len(entries),
	})
}
