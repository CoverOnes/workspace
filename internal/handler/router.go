package handler

import (
	"log/slog"
	"time"

	"github.com/CoverOnes/workspace/internal/platform/health"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// RouterConfig holds all handler-level dependencies.
type RouterConfig struct {
	ContractSvc  *service.ContractService
	SignatureSvc *service.SignatureService
	TaskSvc      *service.TaskService
	WorklogSvc   *service.WorklogService
	Pool         *pgxpool.Pool
	Redis        *redis.Client // may be nil in dev
}

// NewRouter builds and returns the configured Gin engine.
//
// CORS policy: CORS is intentionally NOT applied at this internal service layer.
// workspace is reached only via the API gateway, which owns all browser-facing
// CORS handling. Adding permissive CORS here would widen the attack surface without
// benefit (CONVENTIONS §9 positions CORS after the access-log in the chain but
// the gateway/edge handles it before requests reach this service).
func NewRouter(cfg RouterConfig) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.SetTrustedProxies(nil) //nolint:errcheck // nil proxy list disables proxy trust; gin docs confirm error is always nil for nil argument

	// Global middleware chain (order per CONVENTIONS §9).
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(accessLogger())

	// Health endpoints — registered BEFORE the rate limiter so that liveness /
	// readiness probes are never rate-limited.
	h := health.NewHandler(cfg.Pool)
	r.GET("/healthz", h.Liveness)
	r.GET("/readyz", h.Readiness)

	// Rate limiter — 120 req/min per IP for all API routes below.
	ipRL := middleware.NewIPRateLimiter(cfg.Redis, 120, time.Minute)
	r.Use(ipRL.Handler())

	// All API routes require a valid identity (gateway-injected X-User-Id).
	contractH := NewContractHandler(cfg.ContractSvc)
	signatureH := NewSignatureHandler(cfg.ContractSvc, cfg.SignatureSvc)
	taskH := NewTaskHandler(cfg.TaskSvc)
	worklogH := NewWorklogHandler(cfg.WorklogSvc)

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())

	// Contracts — Tier>=1 for reads, Tier>=2 for writes.
	api.POST("/contracts", middleware.RequireTier(2), contractH.Create)
	api.GET("/contracts", middleware.RequireTier(1), contractH.List)
	api.GET("/contracts/:id", middleware.RequireTier(1), contractH.GetByID)
	api.PATCH("/contracts/:id", middleware.RequireTier(2), contractH.Patch)
	api.POST("/contracts/:id/submit", middleware.RequireTier(2), contractH.Submit)
	api.POST("/contracts/:id/sign", middleware.RequireTier(2), signatureH.Sign)
	api.GET("/contracts/:id/signatures", middleware.RequireTier(1), signatureH.ListSignatures)
	api.POST("/contracts/:id/complete", middleware.RequireTier(2), contractH.Complete)
	api.POST("/contracts/:id/cancel", middleware.RequireTier(2), contractH.Cancel)

	// Tasks.
	api.POST("/contracts/:id/tasks", middleware.RequireTier(2), taskH.CreateTask)
	api.GET("/contracts/:id/tasks", middleware.RequireTier(1), taskH.ListTasks)
	api.PATCH("/contracts/:id/tasks/:taskId", middleware.RequireTier(2), taskH.UpdateTask)
	api.DELETE("/contracts/:id/tasks/:taskId", middleware.RequireTier(2), taskH.DeleteTask)

	// Worklogs.
	api.POST("/contracts/:id/worklogs", middleware.RequireTier(2), worklogH.CreateWorklog)
	api.GET("/contracts/:id/worklogs", middleware.RequireTier(1), worklogH.ListWorklogs)
	api.DELETE("/contracts/:id/worklogs/:worklogId", middleware.RequireTier(2), worklogH.DeleteWorklog)

	return r
}

// accessLogger returns a minimal slog-based access-log middleware.
// Health probe paths (/healthz, /readyz) are excluded to keep logs noise-free.
func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/healthz" || path == "/readyz" {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()
		slog.Info(
			"http",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
		)
	}
}
