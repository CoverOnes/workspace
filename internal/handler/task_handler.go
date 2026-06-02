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

// TaskHandler handles task CRUD endpoints.
type TaskHandler struct {
	svc *service.TaskService
}

// NewTaskHandler returns a TaskHandler.
func NewTaskHandler(svc *service.TaskService) *TaskHandler {
	return &TaskHandler{svc: svc}
}

// CreateTaskRequest is the POST /v1/contracts/:id/tasks request body.
type CreateTaskRequest struct {
	Title          string  `json:"title"`
	AssigneeUserID *string `json:"assigneeUserId"`
}

// CreateTask handles POST /v1/contracts/:id/tasks.
func (h *TaskHandler) CreateTask(c *gin.Context) {
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

	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := service.CreateTaskInput{
		ContractID: contractID,
		CallerID:   identity.UserID,
		Title:      req.Title,
	}

	if req.AssigneeUserID != nil {
		assignee, parseErr := uuid.Parse(*req.AssigneeUserID)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid assigneeUserId")
			return
		}

		in.AssigneeUserID = &assignee
	}

	task, err := h.svc.CreateTask(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, task)
}

// ListTasks handles GET /v1/contracts/:id/tasks.
func (h *TaskHandler) ListTasks(c *gin.Context) {
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

	tasks, err := h.svc.ListTasks(c.Request.Context(), contractID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if tasks == nil {
		tasks = []*domain.Task{}
	}

	httpx.OK(c, tasks)
}

// UpdateTaskRequest is the PATCH /v1/contracts/:id/tasks/:taskId request body.
type UpdateTaskRequest struct {
	Title          *string `json:"title"`
	Status         *string `json:"status"`
	AssigneeUserID *string `json:"assigneeUserId"`
	ClearAssignee  bool    `json:"clearAssignee"`
}

// UpdateTask handles PATCH /v1/contracts/:id/tasks/:taskId.
func (h *TaskHandler) UpdateTask(c *gin.Context) {
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

	taskID, err := uuid.Parse(c.Param("taskId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid task id")
		return
	}

	var req UpdateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := &service.UpdateTaskInput{
		ContractID:    contractID,
		TaskID:        taskID,
		CallerID:      identity.UserID,
		Title:         req.Title,
		ClearAssignee: req.ClearAssignee,
	}

	if req.Status != nil {
		s := domain.TaskStatus(*req.Status)
		in.Status = &s
	}

	if req.AssigneeUserID != nil {
		assignee, parseErr := uuid.Parse(*req.AssigneeUserID)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid assigneeUserId")
			return
		}

		in.AssigneeUserID = &assignee
	}

	task, err := h.svc.UpdateTask(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, task)
}

// DeleteTask handles DELETE /v1/contracts/:id/tasks/:taskId.
func (h *TaskHandler) DeleteTask(c *gin.Context) {
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

	taskID, err := uuid.Parse(c.Param("taskId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid task id")
		return
	}

	if err := h.svc.DeleteTask(c.Request.Context(), contractID, taskID, identity.UserID); err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.NoContent(c)
}
