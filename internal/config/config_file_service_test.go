package config_test

// Tests for the file service configuration validation (validateFileService).
// These verify that FILE_BASE_URL and WORKSPACE_FILE_S2S_TOKEN
// are correctly validated across development and non-development environments.

import (
	"testing"

	"github.com/CoverOnes/workspace/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validFileToken is a 32-character token satisfying the minimum entropy requirement.
const validFileToken = "00000000-aaaa-bbbb-cccc-111111111111"

// validFileURL is a valid https base URL for the file service.
const validFileURL = "https://file-svc.internal"

// setValidDevFileEnv sets up a valid development environment (WORKSPACE_ENV=development)
// so we can test file service validation in isolation without needing a gateway HMAC secret.
func setValidDevFileEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("WORKSPACE_PORT", "8082")
	t.Setenv("WORKSPACE_LOG_LEVEL", "INFO")
	t.Setenv("WORKSPACE_ENV", "development")
	t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", "")
	t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", "")
	t.Setenv("FILE_BASE_URL", "")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", "")
}

// setValidProdFileEnv sets up a valid production environment (needs HMAC + service token).
func setValidProdFileEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WORKSPACE_POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("WORKSPACE_PORT", "8082")
	t.Setenv("WORKSPACE_LOG_LEVEL", "INFO")
	t.Setenv("WORKSPACE_ENV", "production")
	t.Setenv("WORKSPACE_CONTRACT_SERVICE_TOKEN", validServiceToken)
	t.Setenv("WORKSPACE_GATEWAY_HMAC_SECRET", testHMACSecret)
	t.Setenv("FILE_BASE_URL", "")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", "")
}

// --- Happy-path tests ---

func TestFileService_BothUnset_Dev_NoError(t *testing.T) {
	// Neither URL nor token: proof generation disabled — valid in dev.
	setValidDevFileEnv(t)

	cfg, err := config.Load()

	require.NoError(t, err, "both unset in dev must be valid (proof disabled)")
	assert.False(t, cfg.FileServiceEnabled(), "FileServiceEnabled must be false when both are unset")
}

func TestFileService_BothUnset_Prod_NoError(t *testing.T) {
	// Neither URL nor token: proof generation disabled — also valid in prod.
	setValidProdFileEnv(t)

	_, err := config.Load()

	require.NoError(t, err, "both unset in prod must be valid (proof disabled, optional feature)")
}

func TestFileService_BothSet_Valid(t *testing.T) {
	// Both URL and token set with valid values: proof generation enabled.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", validFileURL)
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.True(t, cfg.FileServiceEnabled(), "FileServiceEnabled must be true when both are set")
	assert.Equal(t, validFileURL, cfg.FileBaseURL)
	assert.Equal(t, validFileToken, cfg.FileS2SToken)
}

func TestFileService_HTTPBaseURL_Allowed(t *testing.T) {
	// http:// (not https) is allowed — e.g. for internal k8s service mesh without TLS.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://file-svc.internal")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.NoError(t, err, "http:// base URL must be allowed (internal k8s mesh)")
}

// --- Error cases ---

func TestFileService_InvalidURL_NotHTTP(t *testing.T) {
	// URL set but scheme is not http or https.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "ftp://file-svc.internal")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "non-http/https URL must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

func TestFileService_MalformedURL(t *testing.T) {
	// URL set but completely invalid.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "://not-a-url")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "malformed URL must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

func TestFileService_ShortToken_Rejected(t *testing.T) {
	// Token shorter than 32 chars must always be rejected (entropy floor).
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", validFileURL)
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", "tooshort")

	_, err := config.Load()

	require.Error(t, err, "token shorter than 32 chars must be rejected")
	assert.Contains(t, err.Error(), "WORKSPACE_FILE_S2S_TOKEN")
}

func TestFileService_ShortToken_AlsoRejectedWithoutURL(t *testing.T) {
	// Token set alone (no URL) but shorter than 32 chars — still rejected.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", "short")

	_, err := config.Load()

	require.Error(t, err, "short token must be rejected even without URL")
	assert.Contains(t, err.Error(), "WORKSPACE_FILE_S2S_TOKEN")
}

func TestFileService_URLSetWithoutToken_NonDev_Rejected(t *testing.T) {
	// In non-dev: if URL is set, token is REQUIRED.
	setValidProdFileEnv(t)
	t.Setenv("FILE_BASE_URL", validFileURL)
	// Token intentionally NOT set.

	_, err := config.Load()

	require.Error(t, err, "URL without token in non-dev must be rejected")
	assert.Contains(t, err.Error(), "WORKSPACE_FILE_S2S_TOKEN")
}

func TestFileService_URLSetWithoutToken_Dev_Allowed(t *testing.T) {
	// In dev: URL without token is allowed (proof generation runs but without auth).
	// However the file client will fail at runtime — this is a dev convenience.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", validFileURL)
	// Token intentionally NOT set.

	_, err := config.Load()

	require.NoError(t, err, "URL without token in dev must be allowed (token is optional in dev)")
}

func TestFileService_FileServiceEnabled_BothRequired(t *testing.T) {
	// FileServiceEnabled() requires BOTH base URL and token.
	tests := []struct {
		name    string
		url     string
		token   string
		enabled bool
	}{
		{"both_set", validFileURL, validFileToken, true},
		{"only_url", validFileURL, "", false},
		{"only_token", "", validFileToken, false},
		{"both_empty", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidDevFileEnv(t)
			t.Setenv("FILE_BASE_URL", tc.url)
			t.Setenv("WORKSPACE_FILE_S2S_TOKEN", tc.token)

			// We expect the "only_token" case to fail (short token ""), so Load() may error.
			// We only check FileServiceEnabled() on valid configs.
			cfg, err := config.Load()
			if err != nil {
				// Expected for invalid combos like only_token with no URL but a token value.
				// Skip the FileServiceEnabled() assertion for error cases.
				return
			}

			assert.Equal(t, tc.enabled, cfg.FileServiceEnabled(),
				"FileServiceEnabled() = %v, want %v (url=%q token=%q)",
				cfg.FileServiceEnabled(), tc.enabled, tc.url, tc.token)
		})
	}
}

// --- Whitespace-only token tests (item 4) ---

func TestFileService_WhitespaceOnlyToken_Rejected(t *testing.T) {
	// 32 spaces must fail: whitespace padding does not satisfy the entropy floor.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", validFileURL)
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", "                                ") // 32 spaces

	_, err := config.Load()

	require.Error(t, err, "32-space token must be rejected")
	assert.Contains(t, err.Error(), "WORKSPACE_FILE_S2S_TOKEN")
}

func TestFileService_TabOnlyToken_Rejected(t *testing.T) {
	// Token made of whitespace tabs only must fail.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", validFileURL)
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", "\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t")

	_, err := config.Load()

	require.Error(t, err, "whitespace-tab-only token must be rejected")
	assert.Contains(t, err.Error(), "WORKSPACE_FILE_S2S_TOKEN")
}

// --- SSRF validation tests (item 3) ---

func TestFileService_Loopback_URL_Rejected(t *testing.T) {
	// 127.0.0.1 must always be rejected.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://127.0.0.1:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "loopback URL must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

func TestFileService_IPv6Loopback_URL_Rejected(t *testing.T) {
	// ::1 loopback must be rejected.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://[::1]:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "IPv6 loopback URL must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

func TestFileService_MetadataIP_Rejected(t *testing.T) {
	// 169.254.169.254 (AWS/GCP/Azure instance metadata) must always be rejected.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://169.254.169.254/latest/meta-data")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "metadata IP 169.254.169.254 must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

func TestFileService_MetadataGoogleInternal_Rejected(t *testing.T) {
	// metadata.google.internal must always be rejected.
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://metadata.google.internal/computeMetadata/v1/")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "metadata.google.internal must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

func TestFileService_RFC1918_Prod_Rejected(t *testing.T) {
	// RFC1918 addresses must be rejected in production.
	tests := []struct {
		name string
		url  string
	}{
		{"10.x.x.x", "http://10.0.0.1:9000"},
		{"172.16.x.x", "http://172.16.0.1:9000"},
		{"192.168.x.x", "http://192.168.1.100:9000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidProdFileEnv(t)
			t.Setenv("FILE_BASE_URL", tc.url)
			t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

			_, err := config.Load()

			require.Error(t, err, "RFC1918 URL %s must be rejected in production", tc.url)
			assert.Contains(t, err.Error(), "FILE_BASE_URL")
		})
	}
}

func TestFileService_RFC1918_Dev_Allowed(t *testing.T) {
	// RFC1918 addresses ARE allowed in dev (e.g. local MinIO container).
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://192.168.1.100:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.NoError(t, err, "RFC1918 URL must be allowed in dev")
}

// TestFileService_Localhost_Rejected verifies that "localhost" (hostname string, not an IP)
// is blocked by the loopback guard. net.ParseIP("localhost") returns nil, so without an
// explicit string check the loopback block was bypassed — regression test for that fix.
func TestFileService_Localhost_Rejected(t *testing.T) {
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://localhost:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "localhost URL must be rejected (loopback guard)")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

// TestFileService_LocalhostTrailingDot_Rejected verifies that "localhost." (trailing dot
// in FQDN notation, preserved by url.Hostname()) is also blocked by the explicit string check.
func TestFileService_LocalhostTrailingDot_Rejected(t *testing.T) {
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://localhost.:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "localhost. (trailing dot) URL must be rejected (loopback guard)")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

// TestFileService_MetadataTrailingDot_Rejected verifies that the SSRF metadata-host
// blocklist is applied AFTER stripping the trailing dot so "169.254.169.254." is blocked
// the same as "169.254.169.254".
func TestFileService_MetadataTrailingDot_Rejected(t *testing.T) {
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://169.254.169.254./latest/meta-data")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "169.254.169.254. (trailing-dot FQDN) must be rejected as metadata host")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

// TestFileService_DecimalEncodedLoopback_Rejected verifies that 2130706433 (the decimal
// representation of 127.0.0.1) is rejected as a non-canonical numeric host encoding.
// net.ParseIP("2130706433") returns nil; isNumericHostEncoding catches it deterministically
// (cross-platform — the OS resolver decodes decimal hosts inconsistently).
func TestFileService_DecimalEncodedLoopback_Rejected(t *testing.T) {
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://2130706433:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "decimal-encoded loopback 2130706433 must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

// TestFileService_HexEncodedLoopback_Rejected verifies that 0x7f000001 (hex for
// 127.0.0.1) is rejected as a non-canonical numeric host encoding (cross-platform,
// without relying on the OS resolver).
func TestFileService_HexEncodedLoopback_Rejected(t *testing.T) {
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://0x7f000001:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "hex-encoded loopback 0x7f000001 must be rejected")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

// TestFileService_RFC1918TrailingDot_Prod_Rejected verifies that "10.0.0.1." (trailing
// dot) in production is caught by the private-IP guard after the trailing dot is stripped.
func TestFileService_RFC1918TrailingDot_Prod_Rejected(t *testing.T) {
	setValidProdFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://10.0.0.1.:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "10.0.0.1. (trailing-dot RFC1918) must be rejected in production")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}

// TestFileService_LOCALHOST_CaseInsensitive_Rejected verifies that uppercase LOCALHOST
// is also rejected by the case-insensitive string check.
func TestFileService_LOCALHOST_CaseInsensitive_Rejected(t *testing.T) {
	setValidDevFileEnv(t)
	t.Setenv("FILE_BASE_URL", "http://LOCALHOST:9000")
	t.Setenv("WORKSPACE_FILE_S2S_TOKEN", validFileToken)

	_, err := config.Load()

	require.Error(t, err, "LOCALHOST (uppercase) must be rejected (case-insensitive loopback guard)")
	assert.Contains(t, err.Error(), "FILE_BASE_URL")
}
