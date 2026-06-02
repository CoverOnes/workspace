package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubTaskStoreH struct {
	tasks map[uuid.UUID]*domain.Task
}

func newStubTaskStoreH() *stubTaskStoreH {
	return &stubTaskStoreH{tasks: make(map[uuid.UUID]*domain.Task)}
}

func (s *stubTaskStoreH) Create(_ context.Context, t *domain.Task) error {
	s.tasks[t.ID] = t
	return nil
}

func (s *stubTaskStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, domain.ErrTaskNotFound
	}

	return t, nil
}

func (s *stubTaskStoreH) ListByContract(_ context.Context, contractID uuid.UUID) ([]*domain.Task, error) {
	var result []*domain.Task

	for _, t := range s.tasks {
		if t.ContractID == contractID {
			result = append(result, t)
		}
	}

	return result, nil
}

func (s *stubTaskStoreH) Update(_ context.Context, t *domain.Task) error {
	s.tasks[t.ID] = t
	return nil
}

func (s *stubTaskStoreH) SoftDelete(_ context.Context, id uuid.UUID) error {
	if _, ok := s.tasks[id]; !ok {
		return domain.ErrTaskNotFound
	}

	delete(s.tasks, id)

	return nil
}

func buildTaskRouter(cs *stubContractStoreH, ts *stubTaskStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	svc := service.NewTaskService(cs, ts)
	h := handler.NewTaskHandler(svc)

	r := gin.New()
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())

	api.POST("/contracts/:id/tasks", middleware.RequireTier(2), h.CreateTask)
	api.GET("/contracts/:id/tasks", middleware.RequireTier(1), h.ListTasks)
	api.PATCH("/contracts/:id/tasks/:taskId", middleware.RequireTier(2), h.UpdateTask)
	api.DELETE("/contracts/:id/tasks/:taskId", middleware.RequireTier(2), h.DeleteTask)

	return r
}

func TestTaskHandler_CreateTask(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusActive)
	cs := newStubContractStoreH(contract)
	ts := newStubTaskStoreH()
	r := buildTaskRouter(cs, ts)

	tests := []struct {
		name       string
		callerID   uuid.UUID
		tier       string
		title      string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "party creates task",
			callerID:   clientID,
			tier:       "2",
			title:      "Implement feature",
			wantStatus: http.StatusCreated,
		},
		{
			name:       "non-party gets 404",
			callerID:   thirdPartyID,
			tier:       "2",
			title:      "Task",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		{
			name:       "empty title returns 400",
			callerID:   clientID,
			tier:       "2",
			title:      "",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body, _ := json.Marshal(map[string]any{"title": tc.title})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/v1/contracts/"+contract.ID.String()+"/tasks", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", tc.callerID.String())
			req.Header.Set("X-Kyc-Tier", tc.tier)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestTaskHandler_DeleteTask_Returns204(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusActive)
	cs := newStubContractStoreH(contract)
	ts := newStubTaskStoreH()
	r := buildTaskRouter(cs, ts)

	// Create task via API.
	createBody, _ := json.Marshal(map[string]any{"title": "Delete me"})
	createReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/contracts/"+contract.ID.String()+"/tasks", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-User-Id", clientID.String())
	createReq.Header.Set("X-Kyc-Tier", "2")

	createW := httptest.NewRecorder()
	r.ServeHTTP(createW, createReq)
	require.Equal(t, http.StatusCreated, createW.Code)

	var createResp map[string]any
	require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))
	data := createResp["data"].(map[string]any)
	taskID := data["id"].(string)

	// Delete task.
	deleteReq := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
		"/v1/contracts/"+contract.ID.String()+"/tasks/"+taskID, nil)
	deleteReq.Header.Set("X-User-Id", clientID.String())
	deleteReq.Header.Set("X-Kyc-Tier", "2")

	deleteW := httptest.NewRecorder()
	r.ServeHTTP(deleteW, deleteReq)

	assert.Equal(t, http.StatusNoContent, deleteW.Code)
}
