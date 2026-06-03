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

	// Redis (optional — nil Redis = event publish noop + in-process rate limiter)
	RedisURL string `mapstructure:"redis_url"`

	// Log level: DEBUG, INFO, WARN, ERROR
	LogLevel string `mapstructure:"log_level"`

	// Environment: development | production
	Env string `mapstructure:"env"`

	// AutoMigrate, when true, runs embedded *.up.sql migrations at boot.
	// Intended for local development and CI only; production should use 'task migrate'.
	AutoMigrate bool `mapstructure:"auto_migrate"`
}

// Load reads configuration from environment variables (prefix WORKSPACE_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("WORKSPACE")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":            "WORKSPACE_PORT",
		"postgres_dsn":    "WORKSPACE_POSTGRES_DSN",
		"postgres_schema": "WORKSPACE_DB_SCHEMA",
		"redis_url":       "WORKSPACE_REDIS_URL",
		"log_level":       "WORKSPACE_LOG_LEVEL",
		"env":             "WORKSPACE_ENV",
		"auto_migrate":    "WORKSPACE_AUTO_MIGRATE",
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8082)
	v.SetDefault("log_level", "INFO")
	v.SetDefault("env", "development")

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

	if len(errs) > 0 {
		return errors.New("config validation failed: " + strings.Join(errs, "; "))
	}

	return nil
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
