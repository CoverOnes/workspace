package config_test

import (
	"strings"
	"testing"

	"github.com/CoverOnes/workspace/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("WORKSPACE_PORT", "8082")
	t.Setenv("WORKSPACE_LOG_LEVEL", "INFO")
	t.Setenv("WORKSPACE_ENV", "development")
}

func TestLoad_MissingDSN(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WORKSPACE_POSTGRES_DSN", "")

	_, err := config.Load()
	require.Error(t, err, "Load() must fail when WORKSPACE_POSTGRES_DSN is empty")
	assert.Contains(t, err.Error(), "WORKSPACE_POSTGRES_DSN")
}

func TestLoad_InvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too_large", "99999"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("WORKSPACE_PORT", tc.port)

			_, err := config.Load()
			require.Error(t, err, "Load() must fail for port %s", tc.port)
			assert.Contains(t, err.Error(), "WORKSPACE_PORT")
		})
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WORKSPACE_LOG_LEVEL", "VERBOSE")

	_, err := config.Load()
	require.Error(t, err, "Load() must fail for unknown log level")
	assert.Contains(t, err.Error(), "WORKSPACE_LOG_LEVEL")
}

func TestLoad_InvalidEnv(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WORKSPACE_ENV", "staging")

	_, err := config.Load()
	require.Error(t, err, "Load() must fail for unknown env value")
	assert.Contains(t, err.Error(), "WORKSPACE_ENV")
}

func TestLoad_Success_ParsedValues(t *testing.T) {
	t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://user:pass@db:5432/workspace")
	t.Setenv("WORKSPACE_PORT", "9090")
	t.Setenv("WORKSPACE_LOG_LEVEL", "DEBUG")
	t.Setenv("WORKSPACE_ENV", "production")
	t.Setenv("WORKSPACE_REDIS_URL", "redis://localhost:6379")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@db:5432/workspace", cfg.PostgresDSN)
	assert.Equal(t, 9090, cfg.Port)
	assert.Equal(t, "DEBUG", cfg.LogLevel)
	assert.Equal(t, "production", cfg.Env)
	assert.Equal(t, "redis://localhost:6379", cfg.RedisURL)
}

func TestLoad_Defaults_Applied(t *testing.T) {
	t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	// Clear optional fields so defaults apply.
	t.Setenv("WORKSPACE_PORT", "")
	t.Setenv("WORKSPACE_LOG_LEVEL", "")
	t.Setenv("WORKSPACE_ENV", "")

	// Default port 8082, log_level INFO, env development should be applied.
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 8082, cfg.Port)
	assert.Equal(t, strings.ToUpper("INFO"), strings.ToUpper(cfg.LogLevel))
	assert.Equal(t, "development", cfg.Env)
}

func TestIsDev(t *testing.T) {
	tests := []struct {
		env   string
		isDev bool
	}{
		{"development", true},
		{"DEVELOPMENT", true},
		{"production", false},
		{"test", false},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
			t.Setenv("WORKSPACE_PORT", "8082")
			t.Setenv("WORKSPACE_LOG_LEVEL", "INFO")
			t.Setenv("WORKSPACE_ENV", tc.env)

			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.isDev, cfg.IsDev(), "IsDev() for env=%s", tc.env)
		})
	}
}

func TestLoad_PostgresSchema(t *testing.T) {
	tests := []struct {
		name      string
		schema    string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "empty schema allowed (public default)",
			schema:  "",
			wantErr: false,
		},
		{
			name:    "valid alphanumeric schema",
			schema:  "workspace",
			wantErr: false,
		},
		{
			name:    "valid schema with underscore",
			schema:  "dev_test_schema",
			wantErr: false,
		},
		{
			name:      "schema with hyphen rejected",
			schema:    "my-schema",
			wantErr:   true,
			errSubstr: "WORKSPACE_DB_SCHEMA",
		},
		{
			name:      "schema with dot rejected",
			schema:    "public.contracts",
			wantErr:   true,
			errSubstr: "WORKSPACE_DB_SCHEMA",
		},
		{
			name:      "schema with semicolon rejected (SQL injection attempt)",
			schema:    "workspace;DROP TABLE contracts",
			wantErr:   true,
			errSubstr: "WORKSPACE_DB_SCHEMA",
		},
		{
			name:      "schema with leading digit rejected (invalid PG identifier)",
			schema:    "1workspace",
			wantErr:   true,
			errSubstr: "WORKSPACE_DB_SCHEMA",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("WORKSPACE_DB_SCHEMA", tc.schema)

			cfg, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.schema, cfg.PostgresSchema)
			}
		})
	}
}
