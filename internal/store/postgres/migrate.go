package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunMigrations applies all embedded *.up.sql files against the provided pool in
// lexicographic order. It is idempotent only in the sense that SQL files MUST be
// written to be re-runnable (use IF NOT EXISTS / CREATE TABLE IF NOT EXISTS etc.).
//
// This is intended for WORKSPACE_AUTO_MIGRATE=true (local dev / CI) only.
// Production deployments should run 'task migrate' using the golang-migrate CLI.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
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
		return fmt.Errorf("walk embedded migrations: %w", err)
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", file, readErr)
		}

		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply migration %s: %w", file, execErr)
		}
	}

	return nil
}
