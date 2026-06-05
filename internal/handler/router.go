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
	ContractSvc           *service.ContractService
	SignatureSvc          *service.SignatureService
	TaskSvc               *service.TaskService
	WorklogSvc            *service.WorklogService
	MultipartyContractSvc *service.MultipartyContractService
	MilestoneSvc          *service.MilestoneService
	Pool                  *pgxpool.Pool
	Redis                 *redis.Client // may be nil in dev
	// ContractServiceToken is the pre-shared secret that the marketplace service
	// must supply in X-Service-Token to reach the internal contract-create endpoint.
	ContractServiceToken string
	// GatewayHMACSecret is the §24.1 shared secret used to verify the
	// gateway-origin identity signature. Empty == dev posture (verification
	// disabled); config validation guarantees it is non-empty in non-dev.
	GatewayHMACSecret string
}

// NewRouter builds and returns the configured Gin engine.
//
// CORS policy: CORS is intentionally NOT applied at this internal service layer.
// workspace is reached only via the API gateway, which owns all browser-facing
// CORS handling. Adding permissive CORS here would widen the attack surface without
// benefit (CONVENTIONS §9 positions CORS after the access-log in the chain but
// the gateway/edge handles it before requests reach this service).
func NewRouter(cfg *RouterConfig) *gin.Engine {
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

	// Internal service-to-service routes — protected by a shared pre-shared service token
	// via RequireServiceToken (constant-time compare; fails-fast on empty token in non-dev).
	// §24.1 gateway-signature is for gateway→downstream USER requests only and does NOT
	// apply to this S2S path. The security controls here are:
	//   1. RequireServiceToken: HMAC-safe constant-time comparison of X-Service-Token.
	//   2. Network isolation: the /internal/* prefix MUST NOT be routed by the API gateway
	//      — only reachable from within the cluster (sidecar / VPC internal traffic).
	internalContractH := NewInternalContractHandler(cfg.ContractSvc)
	internal := r.Group("/internal/v1")
	internal.Use(middleware.RequireServiceToken(cfg.ContractServiceToken))
	internal.POST("/contracts", internalContractH.Create)

	// Multi-party routes (both S2S internal and authenticated public) share a single
	// handler instance to avoid double-construction.
	if cfg.MultipartyContractSvc != nil {
		multipartyH := NewMultipartyHandler(cfg.MultipartyContractSvc)

		// S2S: create-or-add-party (marketplace calls this when a collaborator is APPROVED).
		internal.POST("/multiparty-contracts", multipartyH.CreateOrAddParty)

		// Public API routes — authenticated users.
		mpAPI := r.Group("/v1/multiparty-contracts")
		mpAPI.Use(middleware.VerifyGatewaySignature(cfg.GatewayHMACSecret))
		mpAPI.Use(middleware.RequireValidIdentity())
		mpAPI.GET("/:id", middleware.RequireTier(1), multipartyH.GetDetail)
		mpAPI.POST("/:id/submit-for-signature", middleware.RequireTier(2), multipartyH.SubmitForSignatures)
		mpAPI.POST("/:id/sign", middleware.RequireTier(2), multipartyH.Sign)
		mpAPI.PATCH("/:id/parties/:partyId/share", middleware.RequireTier(2), multipartyH.UpdatePartyShare)

		// Milestone endpoints — owner-only (Tier>=2, poster IDOR guard in service layer).
		if cfg.MilestoneSvc != nil {
			milestoneH := NewMilestoneHandler(cfg.MilestoneSvc)
			mpAPI.POST("/:id/milestones", middleware.RequireTier(2), milestoneH.AddMilestone)
			mpAPI.GET("/:id/milestones", middleware.RequireTier(1), milestoneH.ListMilestones)
			mpAPI.POST("/:id/milestones/:mid/complete", middleware.RequireTier(2), milestoneH.CompleteMilestone)
		}
	}

	// S2S roster endpoint — registered when MilestoneSvc is available (it owns the
	// existence-check + party-list logic). Returns the frozen ACTIVE-party
	// [{vendorUserId, shareBps}] for payment; 404 on unknown contract (prevents phantom
	// contracts from returning an empty roster that payment would use for settlement).
	if cfg.MilestoneSvc != nil {
		internalPartiesH := NewInternalPartiesHandler(cfg.MilestoneSvc)
		internal.GET("/contracts/:id/parties", internalPartiesH.GetParties)
	}

	// All API routes require a valid identity (gateway-injected X-User-Id).
	contractH := NewContractHandler(cfg.ContractSvc)
	signatureH := NewSignatureHandler(cfg.ContractSvc, cfg.SignatureSvc)
	taskH := NewTaskHandler(cfg.TaskSvc)
	worklogH := NewWorklogHandler(cfg.WorklogSvc)

	api := r.Group("/v1")
	// Defense-in-depth (§24.1): verify the gateway-origin HMAC signature BEFORE
	// RequireValidIdentity trusts any X-User-Id / X-Kyc-Tier / X-Account-Type /
	// X-Email-Verified header. When the secret is empty (dev) this is a no-op
	// passthrough, matching the gateway's dev signing-skip.
	api.Use(middleware.VerifyGatewaySignature(cfg.GatewayHMACSecret))
	api.Use(middleware.RequireValidIdentity())

	// Contracts — Tier>=1 for reads, Tier>=2 for writes.
	// NOTE: POST /v1/contracts is intentionally removed (M-2 fix). Contracts are
	// created exclusively via the internal S2S endpoint POST /internal/v1/contracts,
	// which is called by marketplace after AcceptBid. Clients cannot self-create
	// contracts with arbitrary freelancer/amount values (CWE-915 / CWE-639).
	api.GET("/contracts", middleware.RequireTier(1), contractH.List)
	api.GET("/contracts/:id", middleware.RequireTier(1), contractH.GetByID)
	api.PATCH("/contracts/:id", middleware.RequireTier(2), contractH.Patch)
	// DRAFT -> PENDING_SIGNATURE (client-only). submit-for-signature is the
	// canonical route the web-app calls for the "送出簽署 / Submit for signature"
	// action; /submit is kept as a backward-compatible alias to the same handler.
	api.POST("/contracts/:id/submit-for-signature", middleware.RequireTier(2), contractH.Submit)
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
