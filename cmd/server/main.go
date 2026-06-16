// Command server starts the CoverOnes workspace microservice.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CoverOnes/workspace/internal/config"
	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/fileclient"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/outbox"
	"github.com/CoverOnes/workspace/internal/platform/logger"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "perform a liveness check against /healthz and exit 0/1")
	flag.Parse()

	// Docker HEALTHCHECK mode: GET /healthz and exit immediately.
	if *healthcheck {
		if err := runHealthCheck(); err != nil {
			slog.Error("healthcheck failed", "err", err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// runHealthCheck issues a GET to the local /healthz endpoint.
func runHealthCheck() error {
	port := os.Getenv("WORKSPACE_PORT")
	if port == "" {
		port = "8082"
	}

	url := fmt.Sprintf("http://127.0.0.1:%s/healthz", port)

	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(url) //nolint:noctx // healthcheck is a one-shot process; no request context needed
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close on healthcheck response

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Logger — JSON to stdout (CONVENTIONS §5).
	log := logger.New(cfg.LogLevel)
	slog.SetDefault(log)

	ctx := context.Background()

	// Postgres pool (CONVENTIONS §12).
	// cfg.PostgresSchema is "" by default (public schema); set WORKSPACE_DB_SCHEMA
	// to isolate this service within a shared Aiven database.
	pool, err := postgres.NewPool(ctx, cfg.PostgresDSN, cfg.PostgresSchema, postgres.PoolConfig{
		MaxConns: int32(cfg.DBMaxConns),
		MinConns: int32(cfg.DBMinConns),
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}

	defer pool.Close()

	slog.Info("postgres connected")

	// Optional auto-migrate (WORKSPACE_AUTO_MIGRATE=true). Intended for local dev / CI.
	// Production: run 'task migrate' using the golang-migrate CLI instead.
	if cfg.AutoMigrate {
		slog.Info("auto-migrate enabled — applying embedded migrations")

		if migrErr := postgres.RunMigrations(ctx, pool); migrErr != nil {
			return fmt.Errorf("auto-migrate: %w", migrErr)
		}

		slog.Info("migrations applied successfully")
	}

	// Redis client (optional — nil means noop publisher + in-process rate limiter).
	redisClient, publisher, err := initRedis(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}

	// Store layer.
	contractStore := postgres.NewContractStore(pool)
	signatureStore := postgres.NewSignatureStore(pool)
	taskStore := postgres.NewTaskStore(pool)
	worklogStore := postgres.NewWorklogStore(pool)
	txManager := postgres.NewTxManager(pool)
	auditLogStore := postgres.NewAuditLogStore(pool)
	outboxStore := postgres.NewOutboxStore(pool)

	// Multiparty store layer.
	multipartyContractStore := postgres.NewMultipartyContractStore(pool)
	multipartyPartyStore := postgres.NewMultipartyPartyStore(pool)
	multipartySignatureStore := postgres.NewMultipartySignatureStore(pool)
	multipartyTxManager := postgres.NewMultipartyTxManager(pool)
	addendumStore := postgres.NewAddendumStore(pool)
	milestoneStore := postgres.NewMilestoneStore(pool)
	milestoneTxManager := postgres.NewMilestoneTxManager(pool)

	// File S2S client (optional — nil when FILE_BASE_URL is not set or dev mode without token).
	var fileClient *fileclient.Client
	if cfg.FileBaseURL != "" && cfg.FileS2SToken != "" {
		fileClient = fileclient.New(fileclient.Config{
			BaseURL:         cfg.FileBaseURL,
			ServiceID:       cfg.FileS2SServiceID,
			Token:           cfg.FileS2SToken,
			BlockPrivateIPs: !cfg.IsDev(), // block RFC1918 in prod/staging; allow in dev (local MinIO)
		})
		slog.Info("file S2S client configured", "base_url", cfg.FileBaseURL, "service_id", cfg.FileS2SServiceID)
	} else {
		slog.Info("file S2S client not configured; signature attachments and proof generation disabled")
	}

	// Service layer.
	contractSvc := service.NewContractService(contractStore, signatureStore, txManager, publisher, fileClient, cfg.FileServiceEnabled())
	signatureSvc := service.NewSignatureService(contractStore, signatureStore, fileClient)
	taskSvc := service.NewTaskService(contractStore, taskStore)
	worklogSvc := service.NewWorklogService(contractStore, worklogStore)
	multipartyContractSvc := service.NewMultipartyContractService(
		multipartyContractStore,
		multipartyPartyStore,
		multipartySignatureStore,
		addendumStore,
		multipartyTxManager,
		publisher,
		cfg.FileServiceEnabled(),
	)
	milestoneSvc := service.NewMilestoneService(multipartyContractStore, milestoneStore, multipartyPartyStore, milestoneTxManager, publisher)
	auditLogSvc := service.NewAuditLogService(auditLogStore)

	// Outbox poller — relay unpublished outbox entries to Redis pub/sub.
	// When Redis is unavailable the poller runs in no-op mode (noop publisher).
	var outboxPublisher outbox.Publisher
	if redisClient != nil {
		outboxPublisher = outbox.NewRedisPublisher(redisClient)
	} else {
		outboxPublisher = &outbox.NoopPublisher{}
	}

	outboxPoller := outbox.New(outboxStore, outboxPublisher)

	// Contract proof service (optional — disabled when file service is not configured).
	// Proof generation is triggered via the transactional outbox (ChannelProofGenerationRequired
	// channel), not by a best-effort goroutine. The poller handler invokes GenerateAndStore,
	// which is idempotent and version-aware. At-least-once delivery is safe because
	// GenerateAndStore handles ErrProofAlreadyExists (idempotent skip) and version supersede.
	proofSvc, err := initProofService(cfg, pool, &service.ProofServiceConfig{
		AuditStore:               auditLogStore,
		ContractStore:            contractStore,
		SignatureStore:           signatureStore,
		MultipartyContractStore:  multipartyContractStore,
		MultipartyPartyStore:     multipartyPartyStore,
		MultipartySignatureStore: multipartySignatureStore,
		FileClient:               fileClient,
	}, outboxPoller)
	if err != nil {
		return err
	}

	go outboxPoller.Start(ctx)

	// Router.
	r := handler.NewRouter(&handler.RouterConfig{
		ContractSvc:           contractSvc,
		SignatureSvc:          signatureSvc,
		TaskSvc:               taskSvc,
		WorklogSvc:            worklogSvc,
		MultipartyContractSvc: multipartyContractSvc,
		MilestoneSvc:          milestoneSvc,
		AuditLogSvc:           auditLogSvc,
		ProofSvc:              proofSvc,
		Pool:                  pool,
		Redis:                 redisClient,
		ContractServiceToken:  cfg.ContractServiceToken,
		GatewayHMACSecret:     cfg.GatewayHMACSecret,
		UserRateLimitPerMin:   cfg.UserRateLimitPerMin,
		UserRateLimitBurst:    cfg.UserRateLimitBurst,
		GatewayCIDR:           cfg.GatewayCIDR,
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "addr", srv.Addr)

		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("server listen error", "err", listenErr)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("shutting down gracefully")

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
		return fmt.Errorf("server shutdown: %w", shutdownErr)
	}

	outboxPoller.Stop()
	slog.Info("server stopped")

	return nil
}

// initRedis connects to Redis when redisURL is non-empty, pings, and returns the client
// (nil on unavailability) plus an appropriate events.Publisher.
// Extracted to keep run()'s cyclomatic complexity within the project budget.
func initRedis(ctx context.Context, redisURL string) (*redis.Client, events.Publisher, error) {
	if redisURL == "" {
		return nil, events.NewNoopPublisher(), nil
	}

	opts, parseErr := redis.ParseURL(redisURL)
	if parseErr != nil {
		return nil, nil, fmt.Errorf("parse redis url: %w", parseErr)
	}

	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if pingErr := client.Ping(pingCtx).Err(); pingErr != nil {
		slog.Warn("redis ping failed; event publishing and rate limiting will use noop/fallback", "err", pingErr)
		return nil, events.NewNoopPublisher(), nil
	}

	slog.Info("redis connected")

	return client, events.NewRedisPublisher(client), nil
}

// initProofService constructs the ProofService (when the file service is configured) and
// registers its outbox handler with the poller. Returns nil without error when the file
// service is not enabled. Extracted to keep run()'s cyclomatic complexity in budget.
//
// partial is a ProofServiceConfig without ProofStore (which requires pool).
// pool is used to create postgres.NewContractProofStore.
func initProofService(
	cfg *config.Config,
	pool *pgxpool.Pool,
	partial *service.ProofServiceConfig,
	poller *outbox.Poller,
) (*service.ProofService, error) {
	if !cfg.FileServiceEnabled() {
		slog.Info("contract proof service disabled (FILE_BASE_URL not set)")
		return nil, nil
	}

	partial.ProofStore = postgres.NewContractProofStore(pool)

	proofSvc, proofErr := service.NewProofService(partial)
	if proofErr != nil {
		return nil, fmt.Errorf("construct proof service: %w", proofErr)
	}

	// Register the proof generation handler with the outbox poller so that
	// proof_generation_required entries are processed in-process rather than
	// published to Redis (this channel has no external subscriber).
	// A longer timeout (30 s) is used for this handler vs the default 5 s Redis path
	// because PDF rendering + file upload can take several seconds under load.
	poller.Handle(service.ChannelProofGenerationRequired, func(hCtx context.Context, entry *domain.OutboxEntry) error {
		return service.HandleProofOutboxEntry(hCtx, proofSvc, entry)
	})

	slog.Info("contract proof service enabled")

	return proofSvc, nil
}
