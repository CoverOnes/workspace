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

	"github.com/CoverOnes/workspace/internal/client"
	"github.com/CoverOnes/workspace/internal/config"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/CoverOnes/workspace/internal/platform/logger"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
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

	httpClient := &http.Client{Timeout: 2 * time.Second}

	resp, err := httpClient.Get(url) //nolint:noctx // healthcheck is a one-shot process; no request context needed
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
	var redisClient *redis.Client

	var publisher events.Publisher

	if cfg.RedisURL != "" {
		opts, parseErr := redis.ParseURL(cfg.RedisURL)
		if parseErr != nil {
			return fmt.Errorf("parse redis url: %w", parseErr)
		}

		redisClient = redis.NewClient(opts)

		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		if pingErr := redisClient.Ping(pingCtx).Err(); pingErr != nil {
			slog.Warn("redis ping failed; event publishing and rate limiting will use noop/fallback", "err", pingErr)
			redisClient = nil
		} else {
			slog.Info("redis connected")
		}
	}

	if redisClient != nil {
		publisher = events.NewRedisPublisher(redisClient)
	} else {
		publisher = events.NewNoopPublisher()
	}

	// Store layer.
	contractStore := postgres.NewContractStore(pool)
	signatureStore := postgres.NewSignatureStore(pool)
	taskStore := postgres.NewTaskStore(pool)
	worklogStore := postgres.NewWorklogStore(pool)
	txManager := postgres.NewTxManager(pool)
	auditLogStore := postgres.NewAuditLogStore(pool)

	// Multiparty store layer.
	multipartyContractStore := postgres.NewMultipartyContractStore(pool)
	multipartyPartyStore := postgres.NewMultipartyPartyStore(pool)
	multipartySignatureStore := postgres.NewMultipartySignatureStore(pool)
	multipartyTxManager := postgres.NewMultipartyTxManager(pool)
	addendumStore := postgres.NewAddendumStore(pool)
	milestoneStore := postgres.NewMilestoneStore(pool)
	milestoneTxManager := postgres.NewMilestoneTxManager(pool)

	// Service layer.
	contractSvc := service.NewContractService(contractStore, signatureStore, txManager, publisher)
	signatureSvc := service.NewSignatureService(contractStore, signatureStore)
	taskSvc := service.NewTaskService(contractStore, taskStore)
	worklogSvc := service.NewWorklogService(contractStore, worklogStore)
	multipartyContractSvc := service.NewMultipartyContractService(
		multipartyContractStore,
		multipartyPartyStore,
		multipartySignatureStore,
		addendumStore,
		multipartyTxManager,
		publisher,
	)
	milestoneSvc := service.NewMilestoneService(multipartyContractStore, milestoneStore, multipartyPartyStore, milestoneTxManager, publisher)
	auditLogSvc := service.NewAuditLogService(auditLogStore)

	// Contract proof service (optional — disabled when file service is not configured).
	// Both services are wired after construction to avoid circular dependencies.
	var proofSvc *service.ProofService
	if cfg.FileServiceEnabled() {
		proofStore := postgres.NewContractProofStore(pool)
		fileClient := client.NewHTTPFileClient(
			cfg.FileServiceBaseURL,
			cfg.FileServiceToken,
			&http.Client{Timeout: 15 * time.Second},
		)

		var proofErr error

		proofSvc, proofErr = service.NewProofService(&service.ProofServiceConfig{
			ProofStore:               proofStore,
			AuditStore:               auditLogStore,
			ContractStore:            contractStore,
			SignatureStore:           signatureStore,
			MultipartyContractStore:  multipartyContractStore,
			MultipartyPartyStore:     multipartyPartyStore,
			MultipartySignatureStore: multipartySignatureStore,
			FileClient:               fileClient,
		})
		if proofErr != nil {
			return fmt.Errorf("construct proof service: %w", proofErr)
		}

		// Wire proof generation into the contract services (best-effort post-activation hook).
		contractSvc.WithProofGenerator(proofSvc)
		multipartyContractSvc.WithProofGenerator(proofSvc)

		slog.Info("contract proof service enabled")
	} else {
		slog.Info("contract proof service disabled (WORKSPACE_FILE_SERVICE_BASE_URL not set)")
	}

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

	slog.Info("server stopped")

	return nil
}
