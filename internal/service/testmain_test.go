package service_test

// TestMain starts ONE Postgres testcontainer for the entire service package test run,
// applies all migrations once, and exposes sharedServicePool for all integration tests.
// This replaces per-test container startups (which exhausted Docker within the 120s timeout).
//
// Tests that verify migration rollback behavior (e.g. TestP4_Migration000008_AppliesAndRollsBack)
// still spin their own isolated container because they mutate the schema.

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// sharedServicePool is the singleton pool shared across all service integration tests.
// It is nil in short mode (container never started).
var sharedServicePool *pgxpool.Pool

// TestMain starts ONE Postgres container, applies migrations once, then runs all tests.
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(runServiceMain(m))
}

// runServiceMain is extracted so deferred cleanup runs before os.Exit.
func runServiceMain(m *testing.M) int {
	if testing.Short() {
		return m.Run()
	}

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)

		return 1
	}

	defer func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate container: %v\n", termErr)
		}
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get connection string: %v\n", err)

		return 1
	}

	sharedServicePool, err = postgres.NewPool(ctx, dsn, "", postgres.PoolConfig{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create pool: %v\n", err)

		return 1
	}

	defer sharedServicePool.Close()

	if err := applyServiceMigrations(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)

		return 1
	}

	return m.Run()
}

// applyServiceMigrations runs all embedded *.up.sql files against the shared test DB.
func applyServiceMigrations(ctx context.Context) error {
	var upFiles []string

	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk migrations FS: %w", err)
	}

	if len(upFiles) == 0 {
		return fmt.Errorf("no *.up.sql files found in embedded FS")
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", file, readErr)
		}

		if _, execErr := sharedServicePool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply %s: %w", file, execErr)
		}
	}

	return nil
}
