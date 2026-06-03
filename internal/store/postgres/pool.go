// Package postgres provides pgxpool-based store implementations.
package postgres

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poolSchemaNameRe is the same rule as config.schemaNameRe, duplicated here so
// NewPool is safe-by-construction even if called from outside the normal
// config.Load() path. First char must be letter or underscore (leading digits
// are invalid PG identifiers and would cause a startup DoS).
var poolSchemaNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

const (
	defaultMaxConns = 10
	defaultMinConns = 2
)

// PoolConfig holds connection pool sizing parameters.
// Zero values fall back to safe defaults (MaxConns=10, MinConns=2).
type PoolConfig struct {
	MaxConns int32
	MinConns int32
}

// NewPool creates and validates a pgxpool.Pool with sensible production defaults.
// Connection budget per CONVENTIONS §12 and backend-security-design §5.3.
//
// If schema is non-empty, the pool will:
//  1. Validate the schema name against [a-zA-Z_][a-zA-Z0-9_]* and return an
//     error immediately if invalid (defense-in-depth; config.validate() also
//     enforces this, but NewPool must be safe-by-construction).
//  2. Create the schema (CREATE SCHEMA IF NOT EXISTS) once on startup.
//  3. Set search_path=<schema>, public for every connection via AfterConnect so
//     all queries resolve against the schema first; public is kept as a fallback
//     so extension functions (e.g. pgcrypto) remain resolvable.
//
// Pool connection sizes are controlled by poolCfg; zero values use defaults.
// If schema is empty the pool behaves identically to before (public schema).
func NewPool(ctx context.Context, dsn, schema string, poolCfg PoolConfig) (*pgxpool.Pool, error) {
	if schema != "" && !poolSchemaNameRe.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q: must match ^[a-zA-Z_][a-zA-Z0-9_]*$", schema)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pgx config: %w", err)
	}

	maxConns := poolCfg.MaxConns
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}

	minConns := poolCfg.MinConns
	if minConns <= 0 {
		minConns = defaultMinConns
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	if schema != "" {
		// AfterConnect sets the search_path for every new connection.
		// pgx.Identifier.Sanitize() double-quotes the identifier so PG reserved
		// words (e.g. "user") are accepted without a syntax error (PG error 42601).
		// schema is pre-validated above; quoting is purely for reserved-word safety.
		quotedSchema := pgx.Identifier{schema}.Sanitize()

		cfg.AfterConnect = func(connectCtx context.Context, conn *pgx.Conn) error {
			_, execErr := conn.Exec(connectCtx, "SET search_path = "+quotedSchema+", public")
			if execErr != nil {
				return fmt.Errorf("set search_path=%s,public: %w", schema, execErr)
			}

			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pgxpool: %w", err)
	}

	if schema != "" {
		// Create the schema once on startup (idempotent).
		// pgx.Identifier.Sanitize() double-quotes the name so reserved words work.
		quotedSchema := pgx.Identifier{schema}.Sanitize()
		if _, execErr := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quotedSchema); execErr != nil {
			pool.Close()
			return nil, fmt.Errorf("create schema %q: %w", schema, execErr)
		}
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return pool, nil
}
