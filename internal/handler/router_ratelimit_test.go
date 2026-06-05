package handler_test

// TestMultipartyRateLimit_SharedBudget asserts that the per-user rate limiter
// applies to /v1/multiparty-contracts/* endpoints and shares the same token
// budget with /v1/* endpoints.
//
// Background: the original router mounted GeneralUserRateLimiter only on the
// /v1 api group; the /v1/multiparty-contracts mpAPI group had no per-user
// limiter at all, allowing unlimited requests to multiparty endpoints as a
// silent rate-limit bypass.  This file pins the fix.

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

// buildSharedRLTestRouter mirrors the NewRouter structure for mpAPI + api groups
// sharing a single GeneralUserRateLimiter, without requiring real service dependencies.
// Both groups have a stub handler at their respective test paths.
func buildSharedRLTestRouter(userRL *middleware.GeneralUserRateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	ok := func(c *gin.Context) { c.Status(http.StatusOK) }

	// Mirrors mpAPI — /v1/multiparty-contracts group.
	mpAPI := r.Group("/v1/multiparty-contracts")
	mpAPI.Use(middleware.RequireValidIdentity())
	if userRL != nil {
		mpAPI.Use(userRL.Handler())
	}
	mpAPI.GET("/:id", ok)

	// Mirrors api — /v1 group.
	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	if userRL != nil {
		api.Use(userRL.Handler())
	}
	api.GET("/contracts", ok)

	return r
}

// doSharedRLRequest fires a GET to path with the given X-User-Id header.
func doSharedRLRequest(r *gin.Engine, path, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
		req.Header.Set("X-Kyc-Tier", "1")
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

// TestMultipartyRateLimit_429OnExhaustedBudget verifies that after exhausting the
// per-user token budget on /v1/* endpoints, subsequent requests to
// /v1/multiparty-contracts/* are also denied with 429.  This is the regression
// test for the silent bypass: if the limiter is only mounted on the /v1 group, the
// multiparty group would return 200 here.
func TestMultipartyRateLimit_429OnExhaustedBudget(t *testing.T) {
	t.Parallel()

	// burst=1: token bucket starts with exactly 1 token.
	userRL := middleware.NewGeneralUserRateLimiter(60, 1)
	r := buildSharedRLTestRouter(userRL)

	uid := uuid.New().String()

	// First request to /v1/contracts — consumes the single token.
	w1 := doSharedRLRequest(r, "/v1/contracts", uid)
	require.Equal(t, http.StatusOK, w1.Code, "first request to /v1/contracts should be allowed")

	// Second request to /v1/multiparty-contracts/:id — same user, same limiter.
	// Without the fix (limiter not mounted on mpAPI), this would return 200.
	// With the fix, the shared budget is exhausted and this must return 429.
	fakeID := uuid.New().String()
	w2 := doSharedRLRequest(r, "/v1/multiparty-contracts/"+fakeID, uid)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code,
		"multiparty endpoint must return 429 when the shared per-user budget is exhausted")
	assert.NotEmpty(t, w2.Header().Get("Retry-After"),
		"429 from multiparty endpoint must include Retry-After header")
}

// TestMultipartyRateLimit_IndependentUsers verifies that different users are
// independently limited even when the limiter is shared across route groups.
func TestMultipartyRateLimit_IndependentUsers(t *testing.T) {
	t.Parallel()

	// burst=1: one token per user.
	userRL := middleware.NewGeneralUserRateLimiter(60, 1)
	r := buildSharedRLTestRouter(userRL)

	uid1 := uuid.New().String()
	uid2 := uuid.New().String()

	// Exhaust uid1's budget on the mpAPI group.
	w1 := doSharedRLRequest(r, "/v1/multiparty-contracts/"+uuid.New().String(), uid1)
	require.Equal(t, http.StatusOK, w1.Code, "uid1 first request must be allowed")

	w2 := doSharedRLRequest(r, "/v1/multiparty-contracts/"+uuid.New().String(), uid1)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code, "uid1 must be rate-limited after burst exhausted")

	// uid2 must still have a full token bucket on the /v1 group.
	w3 := doSharedRLRequest(r, "/v1/contracts", uid2)
	assert.Equal(t, http.StatusOK, w3.Code, "uid2 must not be affected by uid1's exhaustion")
}

// TestMultipartyRateLimit_DisabledWhenPerMinZero verifies that when userRL is nil
// (UserRateLimitPerMin == 0), multiparty endpoints are never 429'd by the per-user
// limiter (the IP limiter still applies, but is not tested here).
func TestMultipartyRateLimit_DisabledWhenPerMinZero(t *testing.T) {
	t.Parallel()

	// nil limiter — mirrors cfg.UserRateLimitPerMin == 0.
	r := buildSharedRLTestRouter(nil)
	uid := uuid.New().String()

	for i := range 10 {
		w := doSharedRLRequest(r, "/v1/multiparty-contracts/"+uuid.New().String(), uid)
		assert.Equal(t, http.StatusOK, w.Code,
			"multiparty endpoint must not be rate-limited when limiter is disabled (request %d)", i+1)
	}
}
