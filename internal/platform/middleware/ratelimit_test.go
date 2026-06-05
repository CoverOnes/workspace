package middleware_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRLTestRouter builds a Gin engine with RequireValidIdentity + userRL.Handler()
// pre-mounted so tests exercise the full middleware chain in the correct order.
func newRLTestRouter(userRL *middleware.GeneralUserRateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequireValidIdentity())
	r.Use(userRL.Handler())
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	return r
}

// doRLRequest fires a GET /test with the given X-User-Id header value.
func doRLRequest(r *gin.Engine, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", http.NoBody)
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func TestGeneralUserRateLimiter_AllowWithinBudget(t *testing.T) {
	// 10 req/min, burst 5 — the first 5 requests must all be allowed because
	// the token bucket starts full at burst capacity.
	userRL := middleware.NewGeneralUserRateLimiter(10, 5)
	r := newRLTestRouter(userRL)

	uid := uuid.New().String()

	for i := range 5 {
		w := doRLRequest(r, uid)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should be allowed within burst budget", i+1)
	}
}

func TestGeneralUserRateLimiter_DenyWhenOverBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		limitPerMin int
		burst       int
	}{
		// burst=1: only 1 token initially; the second request finds the bucket empty.
		{"perMin=60 burst=1", 60, 1},
		// Default config (perMin=120): with the ceil fix, Retry-After must be "1" not "0".
		{"perMin=120 burst=1 (default rate, zero-truncation regression)", 120, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			userRL := middleware.NewGeneralUserRateLimiter(tc.limitPerMin, tc.burst)
			r := newRLTestRouter(userRL)

			uid := uuid.New().String()

			w1 := doRLRequest(r, uid)
			require.Equal(t, http.StatusOK, w1.Code, "first request should be allowed")

			w2 := doRLRequest(r, uid)
			assert.Equal(t, http.StatusTooManyRequests, w2.Code, "second request must be denied after burst exhausted")

			retryAfterStr := w2.Header().Get("Retry-After")
			assert.NotEmpty(t, retryAfterStr, "429 response must include Retry-After header")

			// Retry-After MUST be >= 1 — "0" would tell clients to retry immediately,
			// which defeats the purpose of the header and was the pre-fix bug for
			// any limitPerMin > 60 (including the default 120).
			retryAfterVal, parseErr := strconv.Atoi(retryAfterStr)
			require.NoError(t, parseErr, "Retry-After header must be a valid integer, got %q", retryAfterStr)
			assert.GreaterOrEqual(t, retryAfterVal, 1, "Retry-After must be >= 1 second (got %d for limitPerMin=%d)", retryAfterVal, tc.limitPerMin)

			// Response envelope: {"error": {"code": "RATE_LIMITED", "message": "..."}}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &body))
			assert.Equal(t, "RATE_LIMITED", body.Error.Code, "error code must be RATE_LIMITED")
		})
	}
}

func TestGeneralUserRateLimiter_IndependentBucketsPerUser(t *testing.T) {
	// Two different user IDs must have independent token buckets; exhausting
	// one user's budget must not affect the other user.
	userRL := middleware.NewGeneralUserRateLimiter(60, 1)
	r := newRLTestRouter(userRL)

	uid1 := uuid.New().String()
	uid2 := uuid.New().String()

	// Exhaust uid1's burst bucket.
	doRLRequest(r, uid1) // consumes the single token

	w := doRLRequest(r, uid1)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "uid1 should be rate-limited after burst exhausted")

	// uid2 must still have a full bucket — its first request must pass.
	w2 := doRLRequest(r, uid2)
	assert.Equal(t, http.StatusOK, w2.Code, "uid2 must not be affected by uid1's rate limit")
}

func TestGeneralUserRateLimiter_MissingIdentityPassesThrough(t *testing.T) {
	// Verifies the belt-and-suspenders pass-through branch in Handler() when no
	// verified identity exists in context. We mount the limiter WITHOUT
	// RequireValidIdentity to reach the pass-through branch directly.
	gin.SetMode(gin.TestMode)

	userRL := middleware.NewGeneralUserRateLimiter(60, 1)

	r := gin.New()
	r.Use(userRL.Handler()) // no RequireValidIdentity → identity absent
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Absent identity → pass-through (must not be 429).
	assert.Equal(t, http.StatusOK, w.Code, "missing identity must not trigger rate-limit denial")
}

func TestGeneralUserRateLimiter_ManyDistinctKeysNoPanic(t *testing.T) {
	// Fires requests from many distinct user IDs to confirm the LRU-bounded
	// cache handles eviction gracefully and does not panic.
	userRL := middleware.NewGeneralUserRateLimiter(600, 10)
	r := newRLTestRouter(userRL)

	const distinctUsers = 500

	for i := range distinctUsers {
		// Construct deterministic non-nil UUIDs. i+1 ensures we never produce uuid.Nil
		// (all-zeros), which would trigger the pass-through branch instead of the limiter.
		uid := fmt.Sprintf("00000000-0000-0000-0000-%012x", i+1)
		w := doRLRequest(r, uid)
		// Each user's first request must be allowed (full bucket at burst capacity).
		assert.Equal(t, http.StatusOK, w.Code, "user %d first request must be allowed", i)
	}
}
