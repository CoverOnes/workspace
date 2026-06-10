package config_test

import (
	"strings"
	"testing"

	"github.com/CoverOnes/workspace/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validServiceToken is a 32-character token satisfying the minimum-entropy requirement.
const validServiceToken = "00000000-0000-0000-0000-000000000000"

// testHMACSecret is a 32-char placeholder HMAC secret used in tests only — not a real secret.
const testHMACSecret = "0123456789abcdef0123456789abcdef"

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("WORKSPACE_PORT", "8082")
	t.Setenv("WORKSPACE_LOG_LEVEL", "INFO")
	t.Setenv("WORKSPACE_ENV", "development")
	t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)
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
	// "prod" is not a valid env value (must be one of development|staging|production|test).
	t.Setenv("WORKSPACE_ENV", "prod")

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
	t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)
	t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", testHMACSecret)

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
	t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)
	// Default env = production requires a gateway HMAC secret.
	t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", testHMACSecret)
	// Clear optional fields so defaults apply.
	t.Setenv("WORKSPACE_PORT", "")
	t.Setenv("WORKSPACE_LOG_LEVEL", "")
	t.Setenv("WORKSPACE_ENV", "")

	// Default port 8082, log_level INFO, env production (fail-safe) should be applied.
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 8082, cfg.Port)
	assert.Equal(t, strings.ToUpper("INFO"), strings.ToUpper(cfg.LogLevel))
	assert.Equal(t, "production", cfg.Env)
}

// TestLoad_DefaultEnv_IsFailSafeProduction is load-bearing: it directly pins the
// fail-safe default. An unset WORKSPACE_ENV MUST resolve to production (IsDev()==false)
// so that an empty WORKSPACE_CONTRACT_SERVICE_TOKEN is REJECTED at boot. Reverting
// the default in config.go back to "development" makes both sub-assertions fail,
// which is exactly what we want this test to catch.
func TestLoad_DefaultEnv_IsFailSafeProduction(t *testing.T) {
	t.Run("unset env defaults to production, not dev", func(t *testing.T) {
		t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
		t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)
		// Default env = production requires a gateway HMAC secret.
		t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", testHMACSecret)
		t.Setenv("WORKSPACE_PORT", "")
		t.Setenv("WORKSPACE_LOG_LEVEL", "")
		t.Setenv("WORKSPACE_ENV", "")

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, "production", cfg.Env, "unset WORKSPACE_ENV must default to production")
		assert.False(t, cfg.IsDev(), "unset WORKSPACE_ENV must NOT be treated as development")
	})

	t.Run("unset env + empty service token is rejected at boot", func(t *testing.T) {
		t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
		t.Setenv("WORKSPACE_PORT", "")
		t.Setenv("WORKSPACE_LOG_LEVEL", "")
		t.Setenv("WORKSPACE_ENV", "")
		t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", "")

		_, err := config.Load()
		require.Error(t, err, "unset env (=production) with empty service token must fail validation")
		assert.Contains(t, err.Error(), "WORKSPACE_CONTRACT_SERVICE_TOKEN is required in non-development")
	})
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
			t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)
			// Non-dev envs require a gateway HMAC secret (§24.1 fail-closed).
			t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", testHMACSecret)

			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.isDev, cfg.IsDev(), "IsDev() for env=%s", tc.env)
		})
	}
}

func TestLoad_ContractServiceToken(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		token     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid 36-char UUID token in development",
			env:     "development",
			token:   validServiceToken,
			wantErr: false,
		},
		{
			name:    "valid token in production",
			env:     "production",
			token:   validServiceToken,
			wantErr: false,
		},
		{
			name:    "empty token allowed in development (service may boot for local UI work)",
			env:     "development",
			token:   "",
			wantErr: false,
		},
		{
			name:      "empty token rejected in production (S2S endpoint cannot start)",
			env:       "production",
			token:     "",
			wantErr:   true,
			errSubstr: "WORKSPACE_CONTRACT_SERVICE_TOKEN is required in non-development",
		},
		{
			name:      "empty token rejected in test env",
			env:       "test",
			token:     "",
			wantErr:   true,
			errSubstr: "WORKSPACE_CONTRACT_SERVICE_TOKEN is required in non-development",
		},
		{
			name:      "token too short (31 chars) rejected in development",
			env:       "development",
			token:     "1234567890123456789012345678901",
			wantErr:   true,
			errSubstr: "must be at least 32 characters",
		},
		{
			name:      "token too short rejected in production",
			env:       "production",
			token:     "1234567890123456789012345678901",
			wantErr:   true,
			errSubstr: "must be at least 32 characters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("WORKSPACE_ENV", tc.env)
			t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", tc.token)
			// Non-dev envs require a gateway HMAC secret (§24.1 fail-closed).
			// Set it unconditionally so only the service token is the variable under test.
			t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", testHMACSecret)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
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

func TestLoad_UserRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		perMin    string
		burst     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "perMin negative is rejected",
			perMin:    "-1",
			burst:     "10",
			wantErr:   true,
			errSubstr: "WORKSPACE_USER_RATE_LIMIT_PER_MIN must be >= 0",
		},
		{
			name:      "perMin > 0 and burst <= 0 is rejected",
			perMin:    "60",
			burst:     "0",
			wantErr:   true,
			errSubstr: "WORKSPACE_USER_RATE_LIMIT_BURST must be > 0",
		},
		{
			name:    "perMin=0 disabled — burst value is irrelevant, no error",
			perMin:  "0",
			burst:   "0",
			wantErr: false,
		},
		{
			name:    "valid perMin and burst passes",
			perMin:  "120",
			burst:   "20",
			wantErr: false,
		},
		{
			name:      "perMin above upper bound is rejected",
			perMin:    "100001",
			burst:     "10",
			wantErr:   true,
			errSubstr: "WORKSPACE_USER_RATE_LIMIT_PER_MIN must be <=",
		},
		{
			name:      "burst above upper bound is rejected",
			perMin:    "60",
			burst:     "100001",
			wantErr:   true,
			errSubstr: "WORKSPACE_USER_RATE_LIMIT_BURST must be <=",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("WORKSPACE_USER_RATE_LIMIT_PER_MIN", tc.perMin)
			t.Setenv("WORKSPACE_USER_RATE_LIMIT_BURST", tc.burst)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_GatewayCIDR proves that WORKSPACE_GATEWAY_CIDR is validated as a CIDR
// block when set and accepted when empty (dev/default case).
func TestLoad_GatewayCIDR(t *testing.T) {
	tests := []struct {
		name      string
		cidr      string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "empty CIDR allowed (dev default: SetTrustedProxies(nil))",
			cidr:    "",
			wantErr: false,
		},
		{
			name:    "valid IPv4 CIDR is accepted",
			cidr:    "10.0.0.0/16",
			wantErr: false,
		},
		{
			name:    "valid /32 host CIDR is accepted",
			cidr:    "192.168.1.1/32",
			wantErr: false,
		},
		{
			name:    "valid RFC-1918 VPC CIDR is accepted",
			cidr:    "172.16.0.0/12",
			wantErr: false,
		},
		{
			name:      "bare IP address without prefix is rejected",
			cidr:      "10.0.0.1",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_CIDR",
		},
		{
			name:      "garbage string is rejected",
			cidr:      "not-a-cidr",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_CIDR",
		},
		{
			name:      "CIDR with out-of-range octet is rejected",
			cidr:      "256.0.0.0/8",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_CIDR",
		},
		{
			name:      "IPv4 wildcard 0.0.0.0/0 is rejected (IP spoof risk)",
			cidr:      "0.0.0.0/0",
			wantErr:   true,
			errSubstr: "wildcard",
		},
		{
			name:      "IPv6 wildcard ::/0 is rejected (IP spoof risk)",
			cidr:      "::/0",
			wantErr:   true,
			errSubstr: "wildcard",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("WORKSPACE_GATEWAY_CIDR", tc.cidr)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLoad_GatewayHMAC(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		secret    string
		wantErr   bool
		errSubstr string
	}{
		{
			// §24.1: dev may omit the secret (verification disabled, gateway also skips in dev).
			name:    "dev with empty secret is allowed",
			env:     "development",
			secret:  "",
			wantErr: false,
		},
		{
			// §24.1: non-dev MUST have a ≥32-char secret.
			name:      "production without gateway secret fails (fail-closed)",
			env:       "production",
			secret:    "",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev",
		},
		{
			name:      "staging without gateway secret fails (fail-closed)",
			env:       "staging",
			secret:    "",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev",
		},
		{
			// Even in dev a too-short secret is an error (catches typos).
			name:      "dev with too-short secret is rejected",
			env:       "development",
			secret:    "tooshort",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_HMAC_SECRET, when set, must be at least 32 characters",
		},
		{
			name:      "production with too-short secret is rejected",
			env:       "production",
			secret:    "tooshort",
			wantErr:   true,
			errSubstr: "WORKSPACE_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev",
		},
		{
			name:    "production with valid 32-char secret passes",
			env:     "production",
			secret:  testHMACSecret,
			wantErr: false,
		},
		{
			name:    "staging with valid 32-char secret passes",
			env:     "staging",
			secret:  testHMACSecret,
			wantErr: false,
		},
		{
			name:    "dev with valid 32-char secret passes",
			env:     "development",
			secret:  testHMACSecret,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("WORKSPACE_ENV", tc.env)
			t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", tc.secret)
			// Non-dev envs need the contract service token satisfied as well.
			t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
