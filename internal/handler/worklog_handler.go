package handler

import (
	"net/http"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// WorklogHandler handles worklog CRUD endpoints.
type WorklogHandler struct {
	svc *service.WorklogService
}

// NewWorklogHandler returns a WorklogHandler.
func NewWorklogHandler(svc *service.WorklogService) *WorklogHandler {
	return &WorklogHandler{svc: svc}
}

// CreateWorklogRequest is the POST /v1/contracts/:id/worklogs request body.
type CreateWorklogRequest struct {
	Description string  `json:"description"`
	Minutes     int     `json:"minutes"`
	LoggedAt    *string `json:"loggedAt"` // RFC3339; optional, defaults to server now
}

// CreateWorklog handles POST /v1/contracts/:id/worklogs.
func (h *WorklogHandler) CreateWorklog(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	var req CreateWorklogRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := service.CreateWorklogInput{
		ContractID:  contractID,
		UserID:      identity.UserID, // from header; never from body
		Description: req.Description,
		Minutes:     req.Minutes,
	}

	if req.LoggedAt != nil {
		t, parseErr := time.Parse(time.RFC3339, *req.LoggedAt)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "loggedAt must be RFC3339 format")
			return
		}

		in.LoggedAt = &t
	}

	worklog, err := h.svc.CreateWorklog(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, worklog)
}

// ListWorklogs handles GET /v1/contracts/:id/worklogs.
func (h *WorklogHandler) ListWorklogs(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	worklogs, err := h.svc.ListWorklogs(c.Request.Context(), contractID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if worklogs == nil {
		worklogs = []*domain.Worklog{}
	}

	httpx.OK(c, worklogs)
}

// DeleteWorklog handles DELETE /v1/contracts/:id/worklogs/:worklogId.
func (h *WorklogHandler) DeleteWorklog(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	worklogID, err := uuid.Parse(c.Param("worklogId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid worklog id")
		return
	}

	if err := h.svc.DeleteWorklog(c.Request.Context(), contractID, worklogID, identity.UserID); err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.NoContent(c)
}
