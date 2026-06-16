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

type stubWorklogStoreH struct {
	worklogs map[uuid.UUID]*domain.Worklog
}

func newStubWorklogStoreH() *stubWorklogStoreH {
	return &stubWorklogStoreH{worklogs: make(map[uuid.UUID]*domain.Worklog)}
}

func (s *stubWorklogStoreH) Create(_ context.Context, w *domain.Worklog) error {
	s.worklogs[w.ID] = w
	return nil
}

func (s *stubWorklogStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Worklog, error) {
	w, ok := s.worklogs[id]
	if !ok {
		return nil, domain.ErrWorklogNotFound
	}

	return w, nil
}

func (s *stubWorklogStoreH) ListByContract(_ context.Context, contractID uuid.UUID) ([]*domain.Worklog, error) {
	var result []*domain.Worklog

	for _, w := range s.worklogs {
		if w.ContractID == contractID {
			result = append(result, w)
		}
	}

	return result, nil
}

func (s *stubWorklogStoreH) SoftDelete(_ context.Context, id uuid.UUID) error {
	if _, ok := s.worklogs[id]; !ok {
		return domain.ErrWorklogNotFound
	}

	delete(s.worklogs, id)

	return nil
}

func buildWorklogRouter(cs *stubContractStoreH, ws *stubWorklogStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	svc := service.NewWorklogService(cs, ws)
	h := handler.NewWorklogHandler(svc)

	r := gin.New()
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())

	api.POST("/contracts/:id/worklogs", middleware.RequireTier(2), h.CreateWorklog)
	api.GET("/contracts/:id/worklogs", middleware.RequireTier(1), h.ListWorklogs)
	api.DELETE("/contracts/:id/worklogs/:worklogId", middleware.RequireTier(2), h.DeleteWorklog)

	return r
}

func TestWorklogHandler_CreateWorklog(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusActive)
	cs := newStubContractStoreH(contract)
	ws := newStubWorklogStoreH()
	r := buildWorklogRouter(cs, ws)

	tests := []struct {
		name       string
		callerID   uuid.UUID
		minutes    int
		wantStatus int
		wantCode   string
	}{
		{
			name:       "party logs 60 minutes",
			callerID:   freelancerID,
			minutes:    60,
			wantStatus: http.StatusCreated,
		},
		{
			name:       "non-party gets 404",
			callerID:   thirdPartyID,
			minutes:    60,
			wantStatus: http.StatusNotFound,
			wantCode:   testErrCodeNotFound,
		},
		{
			name:       "minutes 0 returns 400",
			callerID:   clientID,
			minutes:    0,
			wantStatus: http.StatusBadRequest,
			wantCode:   testErrCodeValidation,
		},
		{
			name:       "minutes > 1440 returns 400",
			callerID:   clientID,
			minutes:    1441,
			wantStatus: http.StatusBadRequest,
			wantCode:   testErrCodeValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body, _ := json.Marshal(map[string]any{
				"description": "worked on feature",
				"minutes":     tc.minutes,
			})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/v1/contracts/"+contract.ID.String()+"/worklogs", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", tc.callerID.String())
			req.Header.Set("X-Kyc-Tier", "2")

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

func TestWorklogHandler_DeleteWorklog_AuthorOnly(t *testing.T) {
	t.Parallel()

	clientID := uuid.New()
	freelancerID := uuid.New()

	contract := makeHandlerContract(clientID, freelancerID, domain.ContractStatusActive)
	cs := newStubContractStoreH(contract)
	ws := newStubWorklogStoreH()
	r := buildWorklogRouter(cs, ws)

	// Create worklog as freelancer.
	createBody, _ := json.Marshal(map[string]any{
		"description": "my work",
		"minutes":     120,
	})
	createReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/contracts/"+contract.ID.String()+"/worklogs", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-User-Id", freelancerID.String())
	createReq.Header.Set("X-Kyc-Tier", "2")

	createW := httptest.NewRecorder()
	r.ServeHTTP(createW, createReq)
	require.Equal(t, http.StatusCreated, createW.Code)

	var createResp map[string]any
	require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))
	data := createResp["data"].(map[string]any)
	worklogID := data["id"].(string)

	t.Run("client cannot delete freelancer's worklog", func(t *testing.T) {
		deleteReq := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
			"/v1/contracts/"+contract.ID.String()+"/worklogs/"+worklogID, nil)
		deleteReq.Header.Set("X-User-Id", clientID.String())
		deleteReq.Header.Set("X-Kyc-Tier", "2")

		deleteW := httptest.NewRecorder()
		r.ServeHTTP(deleteW, deleteReq)

		assert.Equal(t, http.StatusNotFound, deleteW.Code)
	})

	t.Run("freelancer (author) can delete own worklog", func(t *testing.T) {
		deleteReq := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
			"/v1/contracts/"+contract.ID.String()+"/worklogs/"+worklogID, nil)
		deleteReq.Header.Set("X-User-Id", freelancerID.String())
		deleteReq.Header.Set("X-Kyc-Tier", "2")

		deleteW := httptest.NewRecorder()
		r.ServeHTTP(deleteW, deleteReq)

		assert.Equal(t, http.StatusNoContent, deleteW.Code)
	})
}
