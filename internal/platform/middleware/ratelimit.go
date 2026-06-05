package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// fallbackBurst is the token-bucket burst for the in-process fallback limiter.
const fallbackBurst = 10

// fallbackLRUCap is the maximum number of unique keys tracked by the in-process
// fallback limiter. Bounded LRU prevents memory-DoS from IP rotation attacks.
const fallbackLRUCap = 100_000

// userRateLRUCap is the maximum number of unique user IDs tracked by the
// in-process per-user limiter. Bounding by LRU prevents memory exhaustion under
// high account-rotation attacks.
const userRateLRUCap = 100_000

// RateLimiter is a Redis-backed fixed-window rate limiter with an in-process
// token-bucket fallback that engages when Redis errors (fails safe, not open).
type RateLimiter struct {
	rdb      *redis.Client
	limit    int
	window   time.Duration
	keyFunc  func(c *gin.Context) string
	fallback *fallbackLimiter
}

// fallbackLimiter holds per-IP token buckets for the in-process safety net.
type fallbackLimiter struct {
	mu      sync.Mutex
	buckets *lru.Cache[string, *rate.Limiter]
	r       rate.Limit
	burst   int
}

func newFallbackLimiter(r rate.Limit, burst int) *fallbackLimiter {
	cache, err := lru.New[string, *rate.Limiter](fallbackLRUCap)
	if err != nil {
		// lru.New only errors when cap <= 0, which cannot happen here.
		panic(fmt.Sprintf("fallbackLimiter: unexpected lru.New error: %v", err))
	}

	return &fallbackLimiter{
		buckets: cache,
		r:       r,
		burst:   burst,
	}
}

func (f *fallbackLimiter) allow(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	lim, ok := f.buckets.Get(key)
	if !ok {
		lim = rate.NewLimiter(f.r, f.burst)
		f.buckets.Add(key, lim)
	}

	return lim.Allow()
}

// NewIPRateLimiter builds a limiter keyed by client IP.
func NewIPRateLimiter(rdb *redis.Client, limit int, window time.Duration) *RateLimiter {
	r := rate.Limit(float64(limit) / window.Seconds())

	return &RateLimiter{
		rdb:    rdb,
		limit:  limit,
		window: window,
		keyFunc: func(c *gin.Context) string {
			return fmt.Sprintf("ws:rl:ip:%s", c.ClientIP())
		},
		fallback: newFallbackLimiter(r, fallbackBurst),
	}
}

// Handler returns the Gin middleware function.
func (rl *RateLimiter) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl.rdb == nil {
			key := rl.keyFunc(c)
			if !rl.fallback.allow(key) {
				c.Abort()
				httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

				return
			}

			c.Next()

			return
		}

		key := rl.keyFunc(c)
		ctx := c.Request.Context()

		count, err := rl.increment(ctx, key)
		if err != nil {
			slog.Warn("rate limiter redis error; applying in-process fallback limiter", "err", err)

			if !rl.fallback.allow(key) {
				c.Abort()
				httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

				return
			}

			c.Next()

			return
		}

		if count > rl.limit {
			c.Abort()
			httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

			return
		}

		c.Next()
	}
}

func (rl *RateLimiter) increment(ctx context.Context, key string) (int, error) {
	pipe := rl.rdb.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, rl.window)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}

	return int(incr.Val()), nil
}

// GeneralUserRateLimiter is a per-authenticated-user in-process token-bucket
// rate limiter. Key is derived from the verified identity set by
// RequireValidIdentity (via IdentityFromCtx). When no verified identity is
// present in context — which cannot happen on properly-wired routes since
// RequireValidIdentity always runs before this middleware — the request is
// passed through with a Warn log.
//
// Multi-pod caveat: this is an in-process limiter. Each pod maintains its own
// bucket, so the effective per-user limit across N pods is N×limitPerMin. A
// Redis sliding-window implementation should be added when accurate cross-pod
// enforcement is required.
type GeneralUserRateLimiter struct {
	mu          sync.Mutex
	buckets     *lru.Cache[string, *rate.Limiter]
	r           rate.Limit
	burst       int
	limitPerMin int
}

// NewGeneralUserRateLimiter builds a per-authenticated-user rate limiter keyed
// on the verified user UUID from IdentityFromCtx. limitPerMin is the sustained
// request budget per user per minute; burst is the token-bucket burst size.
// burst must be > 0; caller is responsible for validating this at config load.
func NewGeneralUserRateLimiter(limitPerMin, burst int) *GeneralUserRateLimiter {
	r := rate.Limit(float64(limitPerMin) / 60.0)

	cache, err := lru.New[string, *rate.Limiter](userRateLRUCap)
	if err != nil {
		// lru.New only errors when cap <= 0, which cannot happen here.
		panic(fmt.Sprintf("GeneralUserRateLimiter: unexpected lru.New error: %v", err))
	}

	return &GeneralUserRateLimiter{
		buckets:     cache,
		r:           r,
		burst:       burst,
		limitPerMin: limitPerMin,
	}
}

func (l *GeneralUserRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	lim, ok := l.buckets.Get(key)
	if !ok {
		lim = rate.NewLimiter(l.r, l.burst)
		l.buckets.Add(key, lim)
	}

	return lim.Allow()
}

// Handler returns the Gin middleware function.
// Deny path: over-limit returns 429 RATE_LIMITED with a Retry-After header.
// Pass-through: if the verified identity is absent from context (should never
// happen after RequireValidIdentity), a Warn is logged and the request is
// passed through belt-and-suspenders style — RequireValidIdentity has already
// rejected unauthenticated requests before this middleware runs.
//
// Security: identity is read exclusively via IdentityFromCtx, which returns
// the uuid.UUID stored by RequireValidIdentity — never from a raw header.
// This ensures the rate-limit key is always gateway-verified.
func (l *GeneralUserRateLimiter) Handler() gin.HandlerFunc {
	// math.Ceil ensures Retry-After is at least 1 second for any limitPerMin value,
	// including the default of 120 (60/120 = 0.5 → ceil → 1). Without Ceil, values
	// > 60 req/min truncate to "0", telling clients to retry immediately (wrong).
	retryAfter := strconv.Itoa(max(1, int(math.Ceil(60.0/float64(l.limitPerMin)))))

	return func(c *gin.Context) {
		identity, ok := IdentityFromCtx(c)
		if !ok || identity.UserID == uuid.Nil {
			slog.Warn(
				"GeneralUserRateLimiter: no verified user identity in context; passing through",
				"path", c.Request.URL.Path,
			)
			c.Next()

			return
		}

		key := "workspace:rl:user:" + identity.UserID.String()

		if !l.allow(key) {
			c.Header("Retry-After", retryAfter)
			c.Abort()
			httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

			return
		}

		c.Next()
	}
}
