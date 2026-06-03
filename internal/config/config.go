// Package config handles environment-first configuration loading for the workspace service.
package config

import (
	"errors"
	"fmt"
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

	// Environment: development | production
	Env string `mapstructure:"env"`

	// AutoMigrate, when true, runs embedded *.up.sql migrations at boot.
	// Intended for local development and CI only; production should use 'task migrate'.
	AutoMigrate bool `mapstructure:"auto_migrate"`

	// ContractServiceToken is the shared secret that marketplace must supply in the
	// X-Service-Token header when calling the internal contract-create endpoint.
	// Required; must be at least 32 characters to enforce adequate entropy.
	// Env: WORKSPACE_CONTRACT_SERVICE_TOKEN
	ContractServiceToken string `mapstructure:"contract_service_token"`
}

// Load reads configuration from environment variables (prefix WORKSPACE_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("WORKSPACE")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":                   "WORKSPACE_PORT",
		"postgres_dsn":           "WORKSPACE_POSTGRES_DSN",
		"postgres_schema":        "WORKSPACE_DB_SCHEMA",
		"redis_url":              "WORKSPACE_REDIS_URL",
		"log_level":              "WORKSPACE_LOG_LEVEL",
		"env":                    "WORKSPACE_ENV",
		"auto_migrate":           "WORKSPACE_AUTO_MIGRATE",
		"db_max_conns":           "WORKSPACE_DB_MAX_CONNS",
		"db_min_conns":           "WORKSPACE_DB_MIN_CONNS",
		"contract_service_token": "WORKSPACE_CONTRACT_SERVICE_TOKEN",
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8082)
	v.SetDefault("log_level", "INFO")
	v.SetDefault("env", "development")
	v.SetDefault("db_max_conns", 10)
	v.SetDefault("db_min_conns", 2)

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

	validEnvs := map[string]bool{"development": true, "production": true, "test": true}
	if !validEnvs[strings.ToLower(c.Env)] {
		errs = append(errs, "WORKSPACE_ENV must be development|production|test")
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

	if tokenErr := c.validateServiceToken(); tokenErr != "" {
		errs = append(errs, tokenErr)
	}

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

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
