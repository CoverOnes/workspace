package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testHeaderUserID is the header name used by the identity middleware.
// The production constant (headerUserID) is unexported in package middleware,
// so this external test package must define its own copy.
const testHeaderUserID = "X-User-Id"

func TestRequireValidIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing X-User-Id returns 401",
			headers:    map[string]string{},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:       "invalid UUID returns 401",
			headers:    map[string]string{testHeaderUserID: "not-a-uuid"},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:       "valid UUID passes through",
			headers:    map[string]string{testHeaderUserID: uuid.New().String()},
			wantStatus: http.StatusOK,
		},
		{
			name: "valid UUID with tier passes through",
			headers: map[string]string{
				testHeaderUserID: uuid.New().String(),
				"X-Kyc-Tier":     "2",
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(middleware.RequireValidIdentity())
			r.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", http.NoBody)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}

func TestRequireTier(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		tier       string
		minTier    int
		wantStatus int
	}{
		{
			name:       "tier 2 satisfies minTier 2",
			tier:       "2",
			minTier:    2,
			wantStatus: http.StatusOK,
		},
		{
			name:       "tier 3 satisfies minTier 2",
			tier:       "3",
			minTier:    2,
			wantStatus: http.StatusOK,
		},
		{
			name:       "tier 1 fails minTier 2",
			tier:       "1",
			minTier:    2,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "tier 0 fails minTier 1",
			tier:       "0",
			minTier:    1,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "missing tier header defaults to 0, fails minTier 1",
			tier:       "",
			minTier:    1,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "negative tier defaults to 0, fails minTier 1",
			tier:       "-1",
			minTier:    1,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "absurdly large tier clamped to 0, fails minTier 1",
			tier:       "9999",
			minTier:    1,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(middleware.RequireValidIdentity())
			r.Use(middleware.RequireTier(tc.minTier))
			r.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", http.NoBody)
			req.Header.Set(testHeaderUserID, uuid.New().String())

			if tc.tier != "" {
				req.Header.Set("X-Kyc-Tier", tc.tier)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}

func TestIdentityFromCtx(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("extracts identity from context", func(t *testing.T) {
		var captured middleware.Identity
		var ok bool

		r := gin.New()
		r.Use(middleware.RequireValidIdentity())
		r.GET("/test", func(c *gin.Context) {
			captured, ok = middleware.IdentityFromCtx(c)
			c.Status(http.StatusOK)
		})

		userID := uuid.New()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", http.NoBody)
		req.Header.Set(testHeaderUserID, userID.String())
		req.Header.Set("X-Kyc-Tier", "2")
		req.Header.Set("X-Account-Type", "PROFESSIONAL")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		require.True(t, ok)
		assert.Equal(t, userID, captured.UserID)
		assert.Equal(t, 2, captured.KYCTier)
		assert.Equal(t, "PROFESSIONAL", captured.AccountType)
	})
}
