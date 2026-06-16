// Package config handles environment-first configuration loading for the workspace service.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/joho/godotenv"
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

	// FileBaseURL is the base URL of the CoverOnes file service used for S2S
	// attachment registration, presign requests, and proof PDF storage.
	// Required in non-development environments when the file service is used.
	// Env: FILE_BASE_URL
	FileBaseURL string `mapstructure:"file_base_url"`

	// FileS2SServiceID is the service-ID sent in X-Service-Id when calling the
	// file service. Defaults to "workspace".
	// Env: FILE_S2S_SERVICE_ID
	FileS2SServiceID string `mapstructure:"file_s2s_service_id"`

	// FileS2SToken is the pre-shared token sent in X-Service-Token when calling
	// the file service. NEVER logged or included in URLs.
	// Required in non-development environments (≥32 chars enforced).
	// Env: WORKSPACE_FILE_S2S_TOKEN
	FileS2SToken string `mapstructure:"file_s2s_token"`
}

// Load reads configuration from environment variables (prefix WORKSPACE_).
func Load() (*Config, error) {
	_ = godotenv.Load(".env.local") // local dev/test (optional, does not override existing env)
	_ = godotenv.Load(".env")       // prod fallback (optional, does not override existing env)

	v := viper.New()

	v.SetEnvPrefix("WORKSPACE")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	//nolint:gosec // G101 false positive: map keys are viper config key names (e.g. "file_s2s_token"),
	// not actual credential values; the values are env-var name strings (e.g. "WORKSPACE_FILE_S2S_TOKEN").
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
		"file_base_url":           "FILE_BASE_URL",
		"file_s2s_service_id":     "FILE_S2S_SERVICE_ID",
		"file_s2s_token":          "WORKSPACE_FILE_S2S_TOKEN",
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
	v.SetDefault("file_s2s_service_id", "workspace")
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

	errs = append(errs, c.validateFileService()...)

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

	_, ipNet, err := net.ParseCIDR(c.GatewayCIDR)
	if err != nil {
		return []string{fmt.Sprintf("WORKSPACE_GATEWAY_CIDR must be a valid CIDR block (e.g. 10.0.0.0/16): %v", err)}
	}

	// Reject wildcard CIDRs (0.0.0.0/0, ::/0): trusting all peers lets any client
	// spoof their IP via X-Forwarded-For, defeating signer_ip audit + rate limiting.
	if ones, _ := ipNet.Mask.Size(); ones == 0 {
		return []string{
			"WORKSPACE_GATEWAY_CIDR must not be a wildcard (0.0.0.0/0 or ::/0): " +
				"it lets any client spoof their IP via X-Forwarded-For",
		}
	}

	return nil
}

// minFileTokenLen is the minimum length for the file service S2S token.
// Mirrors validateServiceToken (§24 token entropy floor).
const minFileTokenLen = 32

// ssrfBlockedHosts lists hostname patterns that must be rejected unconditionally to
// prevent SSRF attacks via the file service base URL. Checked before RFC1918 blocks.
var ssrfBlockedHosts = []string{
	"169.254.169.254",          // AWS/GCP/Azure instance metadata endpoint
	"metadata.google.internal", // GCP metadata endpoint alternate hostname
}

// ssrfPrivateNets are RFC1918 + ULA IPv6 ranges rejected in production/staging.
// In dev they are allowed to support local MinIO / file-service containers.
var ssrfPrivateNets = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fc00::/7", // IPv6 ULA
	}

	nets := make([]*net.IPNet, 0, len(cidrs))

	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err == nil {
			nets = append(nets, ipNet)
		}
	}

	return nets
}()

// isNumericHostEncoding reports whether host is a non-standard numeric IP encoding
// (decimal 2130706433, hex 0x7f000001, octal/dotted-numeric 0177.0.0.1) that
// net.ParseIP rejected.  Such forms are never a legitimate file service hostname, and
// the OS resolver decodes them inconsistently across platforms (BSD/macOS inet_aton
// accepts them, Linux glibc getaddrinfo does not) — so we detect them deterministically
// and fail closed rather than rely on the resolver.  A real hostname has at least one
// label containing a non-hex character and is therefore not flagged.
func isNumericHostEncoding(host string) bool {
	if host == "" {
		return false
	}

	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return false // empty label → malformed, treat as hostname (rejected elsewhere)
		}

		l := strings.ToLower(label)

		if rest, ok := strings.CutPrefix(l, "0x"); ok {
			if rest == "" || strings.IndexFunc(rest, isNotHexDigit) >= 0 {
				return false
			}

			continue
		}

		if strings.IndexFunc(l, isNotDecimalDigit) >= 0 {
			return false // a label with a non-digit → real hostname
		}
	}

	return true
}

func isNotHexDigit(r rune) bool {
	return (r < '0' || r > '9') && (r < 'a' || r > 'f')
}

func isNotDecimalDigit(r rune) bool {
	return r < '0' || r > '9'
}

// isBlockedIP reports whether ip is blocked (loopback, link-local, or — when
// blockPrivate is true — RFC1918/ULA).  Also returns a human-readable reason.
// Used by both the boot-time config validator and the runtime dial guard so that
// the same block rules are applied in both places without duplication.
func isBlockedIP(ip net.IP, blockPrivate bool) (blocked bool, reason string) {
	if ip.IsLoopback() {
		return true, "loopback address"
	}

	if ip.IsLinkLocalUnicast() {
		return true, "link-local address"
	}

	if blockPrivate {
		for _, privateNet := range ssrfPrivateNets {
			if privateNet.Contains(ip) {
				return true, "private/RFC1918 address in production"
			}
		}
	}

	return false, ""
}

// validateFileServiceURL validates the parsed URL for SSRF risk.
// Returns an error string if the URL is unsafe, or "" if safe.
// Unconditionally rejects loopback, link-local, and known metadata endpoints.
// In production/staging, also rejects RFC1918 and ULA IPv6 ranges.
//
// Bypass defenses:
//   - Trailing-dot FQDN (e.g. "localhost.", "169.254.169.254.") — stripped before checks.
//   - Decimal/hex-encoded IPs (e.g. 2130706433, 0x7f000001) — rejected by isNumericHostEncoding.
func validateFileServiceURL(rawURL string, isProd bool) string {
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "FILE_BASE_URL must be a valid http or https URL"
	}

	// Strip trailing dot from FQDN notation (e.g. "localhost." → "localhost",
	// "169.254.169.254." → "169.254.169.254") BEFORE any string or IP comparisons.
	host := strings.TrimSuffix(u.Hostname(), ".")

	// Unconditionally block "localhost" by name — net.ParseIP("localhost") returns nil
	// so we must check the string explicitly before the IP path.
	if strings.EqualFold(host, "localhost") {
		return "FILE_BASE_URL must not point to a loopback address"
	}

	// Block known metadata hostnames unconditionally (checked before IP resolution).
	for _, blocked := range ssrfBlockedHosts {
		if strings.EqualFold(host, blocked) {
			return "FILE_BASE_URL must not point to a metadata/SSRF-risk host"
		}
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// Not a canonical IP literal. Reject non-standard numeric encodings deterministically
		// (decimal/hex/octal) — these are never a valid file service host and the OS resolver
		// decodes them inconsistently across platforms. A real hostname (e.g. "file-svc.internal")
		// is allowed at boot; the runtime dial guard validates its resolved IP for ongoing
		// DNS-rebinding protection.
		if isNumericHostEncoding(host) {
			return "FILE_BASE_URL must not use a numeric-encoded host"
		}

		return ""
	}

	if blocked, reason := isBlockedIP(ip, isProd); blocked {
		return "FILE_BASE_URL must not point to a " + reason
	}

	return ""
}

// validateFileService validates FILE_BASE_URL and WORKSPACE_FILE_S2S_TOKEN.
//
// Rules:
//   - If FileBaseURL is not set, file service is disabled — no validation needed.
//   - FileBaseURL, when set, must parse as http or https and must not point to
//     loopback, link-local, metadata, or (in prod/staging) RFC1918 addresses (SSRF guard).
//   - FileBaseURL must be a well-formed absolute URL; a relative URL or bare hostname
//     would fail silently at runtime rather than at boot.
//   - In non-dev: WORKSPACE_FILE_S2S_TOKEN is REQUIRED when FileBaseURL is set,
//     and must be at least 32 characters to enforce adequate entropy.
//   - In dev: token may be omitted (file service not exercised); if set, must be ≥32 chars.
func (c *Config) validateFileService() []string {
	var errs []string

	// Entropy floor: whenever the token is provided it MUST meet the minimum length,
	// even if FILE_BASE_URL is not set. This catches the common operator mistake of
	// configuring a weak token before wiring up the URL.
	if c.FileS2SToken != "" && len(strings.TrimSpace(c.FileS2SToken)) < minFileTokenLen {
		errs = append(errs, fmt.Sprintf("WORKSPACE_FILE_S2S_TOKEN must be at least %d non-whitespace characters", minFileTokenLen))
	}

	if c.FileBaseURL == "" {
		// No file service URL configured — remaining URL/token-required checks are skipped.
		return errs
	}

	// Validate FileBaseURL is a well-formed absolute URL with a scheme.
	// A misconfigured value like "file-service" (no scheme) would fail silently at runtime
	// on the first S2S call rather than at boot, giving a poor operator experience.
	if _, parseErr := url.ParseRequestURI(c.FileBaseURL); parseErr != nil {
		errs = append(errs, fmt.Sprintf("FILE_BASE_URL must be a valid absolute URL (e.g. https://file-service): %v", parseErr))
	} else {
		// Only run SSRF check if the URL parsed successfully.
		if msg := validateFileServiceURL(c.FileBaseURL, !c.IsDev()); msg != "" {
			errs = append(errs, msg)
		}
	}

	// Token required when URL is set in non-dev (token may be omitted in dev only).
	if c.FileS2SToken == "" && !c.IsDev() {
		errs = append(errs, "WORKSPACE_FILE_S2S_TOKEN is required when FILE_BASE_URL is set in non-development environments")
	}

	return errs
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}

// FileServiceEnabled reports whether both FileBaseURL and FileS2SToken are configured,
// meaning the file service S2S client can be used for proof generation, attachment
// registration, and presigned downloads.
func (c *Config) FileServiceEnabled() bool {
	return c.FileBaseURL != "" && c.FileS2SToken != ""
}
