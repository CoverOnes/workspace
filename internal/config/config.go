// Package config handles environment-first configuration loading for the workspace service.
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

// schemaNameRe validates that a Postgres schema name only contains safe characters
// to prevent SQL injection when the name is interpolated into CREATE SCHEMA.
// First character must be a letter or underscore (leading digits are invalid PG identifiers).
var schemaNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Config holds all configuration for the workspace service.
type Config struct {
	// Server
	Port int `mapstructure:"port"`

	// Postgres
	PostgresDSN string `mapstructure:"postgres_dsn"`

	// PostgresSchema is the optional Postgres schema to use (default: "" = public).
	// Set to "workspace" when sharing one Aiven database across multiple services
	// so each service is isolated by schema rather than by database.
	// Only alphanumeric characters and underscores are allowed ([a-zA-Z0-9_]+).
	PostgresSchema string `mapstructure:"postgres_schema"`

	// DBMaxConns is the maximum number of connections in the pgxpool (default: 10).
	// Set WORKSPACE_DB_MAX_CONNS to reduce when sharing a small Aiven plan.
	DBMaxConns int `mapstructure:"db_max_conns"`

	// DBMinConns is the minimum number of idle connections kept alive (default: 2).
	// Set WORKSPACE_DB_MIN_CONNS to 0 for environments with very limited quota.
	DBMinConns int `mapstructure:"db_min_conns"`

	// Redis (optional — nil Redis = event publish noop + in-process rate limiter)
	RedisURL string `mapstructure:"redis_url"`

	// Log level: DEBUG, INFO, WARN, ERROR
	LogLevel string `mapstructure:"log_level"`

	// Environment: development | production | test
	// Defaults to "production" (fail-safe): an unset WORKSPACE_ENV is treated as
	// prod so the S2S service token is REQUIRED. Dev machines MUST set
	// WORKSPACE_ENV=development explicitly.
	Env string `mapstructure:"env"`

	// AutoMigrate, when true, runs embedded *.up.sql migrations at boot.
	// Intended for local development and CI only; production should use 'task migrate'.
	AutoMigrate bool `mapstructure:"auto_migrate"`

	// ContractServiceToken is the shared secret that marketplace must supply in the
	// X-Service-Token header when calling the internal contract-create endpoint.
	// Required; must be at least 32 characters to enforce adequate entropy.
	// Env: WORKSPACE_CONTRACT_SERVICE_TOKEN
	ContractServiceToken string `mapstructure:"contract_service_token"`

	// GatewayHMACSecret is the shared secret used to verify the gateway-origin
	// identity signature (conventions §24.1). It MUST equal the gateway's
	// GATEWAY_HMAC_SECRET. Non-dev (staging/production) fails fast at boot if
	// empty or shorter than 32 chars; development may omit it (verification
	// disabled, mirroring the gateway which also disables signing in dev).
	// chmod 0600 the file that provides it; prefer the env var as canonical.
	// Env: WORKSPACE_GATEWAY_HMAC_SECRET
	GatewayHMACSecret string `mapstructure:"gateway_hmac_secret"`

	// UserRateLimitPerMin is the per-user token-bucket rate limit (requests per minute).
	// Set to 0 to disable per-user rate limiting (IP limiter still applies).
	// Env: WORKSPACE_USER_RATE_LIMIT_PER_MIN
	UserRateLimitPerMin int `mapstructure:"user_rate_limit_per_min"`

	// UserRateLimitBurst is the token-bucket burst size for per-user rate limiting.
	// Must be > 0 when UserRateLimitPerMin > 0.
	// Env: WORKSPACE_USER_RATE_LIMIT_BURST
	UserRateLimitBurst int `mapstructure:"user_rate_limit_burst"`

	// GatewayCIDR is the IP CIDR of the API gateway/load-balancer that forwards
	// requests to this service. When set, Gin is told to trust X-Forwarded-For
	// only from this source, so c.ClientIP() returns the real end-user IP rather
	// than the gateway's IP. This fixes two blocking audit findings:
	//   - IP rate limiter bucket collapses to a single global bucket (self-DoS).
	//   - signer_ip records the gateway IP instead of the real signer.
	// Example: "10.0.0.0/16" (k8s cluster CIDR), "172.16.0.0/12" (VPC internal).
	// Empty (default): trusted-proxy list is nil — c.ClientIP() returns RemoteAddr
	// (safe fallback; use when gateway forwards no X-Forwarded-For).
	// Env: WORKSPACE_GATEWAY_CIDR
	GatewayCIDR string `mapstructure:"gateway_cidr"`
}

// Load reads configuration from environment variables (prefix WORKSPACE_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("WORKSPACE")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":                    "WORKSPACE_PORT",
		"postgres_dsn":            "WORKSPACE_POSTGRES_DSN",
		"postgres_schema":         "WORKSPACE_DB_SCHEMA",
		"redis_url":               "WORKSPACE_REDIS_URL",
		"log_level":               "WORKSPACE_LOG_LEVEL",
		"env":                     "WORKSPACE_ENV",
		"auto_migrate":            "WORKSPACE_AUTO_MIGRATE",
		"db_max_conns":            "WORKSPACE_DB_MAX_CONNS",
		"db_min_conns":            "WORKSPACE_DB_MIN_CONNS",
		"contract_service_token":  "WORKSPACE_CONTRACT_SERVICE_TOKEN",
		"gateway_hmac_secret":     "WORKSPACE_GATEWAY_HMAC_SECRET",
		"user_rate_limit_per_min": "WORKSPACE_USER_RATE_LIMIT_PER_MIN",
		"user_rate_limit_burst":   "WORKSPACE_USER_RATE_LIMIT_BURST",
		"gateway_cidr":            "WORKSPACE_GATEWAY_CIDR",
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8082)
	v.SetDefault("log_level", "INFO")
	// Fail-safe default: an unset WORKSPACE_ENV is treated as production, which
	// makes validateServiceToken() REQUIRE a real WORKSPACE_CONTRACT_SERVICE_TOKEN.
	// If this defaulted to "development", a prod deploy that forgot to set
	// WORKSPACE_ENV would silently boot IsDev()=true, allow an EMPTY service token,
	// and then RequireServiceToken("") would reject every supplied header — making
	// /internal/v1/contracts unreachable while the operator falsely believes a
	// token gate is active. Dev machines / dev-stack set WORKSPACE_ENV=development
	// explicitly.
	v.SetDefault("env", "production")
	v.SetDefault("db_max_conns", 10)
	v.SetDefault("db_min_conns", 2)
	v.SetDefault("user_rate_limit_per_min", 120)
	v.SetDefault("user_rate_limit_burst", 20)

	var cfg Config

	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []string

	if c.PostgresDSN == "" {
		errs = append(errs, "WORKSPACE_POSTGRES_DSN is required")
	}

	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, "WORKSPACE_PORT must be 1-65535")
	}

	validLogLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	if !validLogLevels[strings.ToUpper(c.LogLevel)] {
		errs = append(errs, "WORKSPACE_LOG_LEVEL must be DEBUG|INFO|WARN|ERROR")
	}

	// §24.1 fail-closed env posture: WORKSPACE_ENV MUST be one of the known
	// values. An unknown or empty string (after defaults) is a boot error.
	// 'staging' is added per conventions §24.1 three-tier model.
	validEnvs := map[string]bool{
		"development": true,
		"staging":     true,
		"production":  true,
		"test":        true,
	}
	if !validEnvs[strings.ToLower(c.Env)] {
		errs = append(errs, "WORKSPACE_ENV must be development|staging|production|test")
	}

	if c.PostgresSchema != "" && !schemaNameRe.MatchString(c.PostgresSchema) {
		errs = append(errs, "WORKSPACE_DB_SCHEMA must start with a letter or underscore and contain only [a-zA-Z0-9_] characters")
	}

	if c.DBMaxConns < 0 || c.DBMaxConns > 65535 {
		errs = append(errs, "WORKSPACE_DB_MAX_CONNS must be 0-65535 (0 = use default of 10)")
	}

	if c.DBMinConns < 0 || c.DBMinConns > 65535 {
		errs = append(errs, "WORKSPACE_DB_MIN_CONNS must be 0-65535 (0 = use default of 2)")
	}

	errs = append(errs, c.validateUserRateLimit()...)

	if tokenErr := c.validateServiceToken(); tokenErr != "" {
		errs = append(errs, tokenErr)
	}

	errs = append(errs, c.validateGatewayHMAC()...)

	errs = append(errs, c.validateGatewayCIDR()...)

	if len(errs) > 0 {
		return errors.New("config validation failed: " + strings.Join(errs, "; "))
	}

	return nil
}

// validateServiceToken validates WORKSPACE_CONTRACT_SERVICE_TOKEN and returns an
// error message (empty string = valid).
//
// The internal S2S contract-create endpoint cannot start without this token.
// In non-development environments it is REQUIRED and must have adequate entropy
// (>= 32 chars). In development we allow it to be unset so the service can boot
// for local UI work that does not exercise the internal endpoint — but if it is
// set, it must still meet the length floor (catches typos / truncated secrets).
func (c *Config) validateServiceToken() string {
	const minServiceTokenLen = 32

	switch {
	case c.IsDev() && c.ContractServiceToken == "":
		return "" // allowed: dev boot without the S2S token
	case c.ContractServiceToken == "":
		return "WORKSPACE_CONTRACT_SERVICE_TOKEN is required in non-development environments " +
			"(set it to a random secret of at least 32 characters; the internal contract-create " +
			"endpoint cannot start without it)"
	case len(c.ContractServiceToken) < minServiceTokenLen:
		return "WORKSPACE_CONTRACT_SERVICE_TOKEN must be at least 32 characters"
	default:
		return ""
	}
}

// minHMACSecretLen is the minimum length of the gateway HMAC secret. It mirrors
// the gateway's GATEWAY_HMAC_SECRET ≥32-char requirement (conventions §24.1).
const minHMACSecretLen = 32

// validateGatewayHMAC enforces the §24.1 fail-closed secret posture:
//   - non-dev (staging/production/test): secret is REQUIRED and MUST be ≥32 chars —
//     boot fails fast otherwise (mirrors the gateway which fails fast in non-dev).
//   - dev: secret may be empty (verification disabled, mirroring the gateway's
//     dev signing-skip); but if a secret IS provided it must still be ≥32 chars
//     so a too-short dev secret never masquerades as a valid one.
func (c *Config) validateGatewayHMAC() []string {
	var errs []string

	if !c.IsDev() {
		if len(c.GatewayHMACSecret) < minHMACSecretLen {
			errs = append(errs, "WORKSPACE_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev (staging/production) environments")
		}

		return errs
	}

	// Dev: empty is allowed (verification disabled); non-empty must be ≥32.
	if c.GatewayHMACSecret != "" && len(c.GatewayHMACSecret) < minHMACSecretLen {
		errs = append(errs, "WORKSPACE_GATEWAY_HMAC_SECRET, when set, must be at least 32 characters")
	}

	return errs
}

// maxUserRateLimitPerMin is a sanity cap on per-user rate limits. Values above
// this are almost certainly misconfiguration (e.g. accidentally supplying a
// per-second value or an unbounded integer). The IP limiter already runs at
// 120 req/min; per-user limits above 100 000 are meaningless in practice.
const maxUserRateLimitPerMin = 100_000

// maxUserRateLimitBurst mirrors the cap on the sustained rate.
const maxUserRateLimitBurst = 100_000

// validateUserRateLimit validates per-user rate-limit configuration and returns
// any error messages (empty slice = valid).
//
// Rules:
//   - UserRateLimitPerMin must be >= 0 (0 disables per-user limiting; IP limiter still runs).
//   - UserRateLimitPerMin must be <= 100 000 (sanity cap; prevents accidental bypass).
//   - UserRateLimitBurst must be > 0 when UserRateLimitPerMin > 0, because a zero-burst
//     token bucket would deny every request immediately.
//   - UserRateLimitBurst must be <= 100 000 (matching sanity cap).
//   - UserRateLimitBurst is unchecked when UserRateLimitPerMin == 0 (limiter disabled).
func (c *Config) validateUserRateLimit() []string {
	var errs []string

	if c.UserRateLimitPerMin < 0 {
		errs = append(errs, "WORKSPACE_USER_RATE_LIMIT_PER_MIN must be >= 0 (0 = disabled)")
	}

	if c.UserRateLimitPerMin > maxUserRateLimitPerMin {
		errs = append(errs, fmt.Sprintf("WORKSPACE_USER_RATE_LIMIT_PER_MIN must be <= %d", maxUserRateLimitPerMin))
	}

	if c.UserRateLimitPerMin > 0 && c.UserRateLimitBurst <= 0 {
		errs = append(errs, "WORKSPACE_USER_RATE_LIMIT_BURST must be > 0 when WORKSPACE_USER_RATE_LIMIT_PER_MIN > 0")
	}

	if c.UserRateLimitPerMin > 0 && c.UserRateLimitBurst > maxUserRateLimitBurst {
		errs = append(errs, fmt.Sprintf("WORKSPACE_USER_RATE_LIMIT_BURST must be <= %d", maxUserRateLimitBurst))
	}

	return errs
}

// validateGatewayCIDR validates WORKSPACE_GATEWAY_CIDR and returns any error messages.
// An empty CIDR is valid (trusted-proxy list falls back to nil, safe for local dev).
// A non-empty value must be a valid CIDR block (e.g. "10.0.0.0/16").
// NEVER set to "0.0.0.0/0" — that allows clients to spoof their IP via X-Forwarded-For.
func (c *Config) validateGatewayCIDR() []string {
	if c.GatewayCIDR == "" {
		return nil
	}

	if _, _, err := net.ParseCIDR(c.GatewayCIDR); err != nil {
		return []string{fmt.Sprintf("WORKSPACE_GATEWAY_CIDR must be a valid CIDR block (e.g. 10.0.0.0/16): %v", err)}
	}

	return nil
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
