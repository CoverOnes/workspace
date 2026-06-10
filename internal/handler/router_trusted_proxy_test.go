package handler_test

// Regression tests for the Major audit findings (2026-06-06 five-army audit):
//
//   [Major] router.go:51 — IP rate limiter collapses to a single global bucket
//           behind the gateway because SetTrustedProxies(nil) makes c.ClientIP()
//           return the gateway's IP for every request.
//   [Major] signature_handler.go:75 — signer_ip records the gateway IP instead of
//           the real signer IP for the same root cause.
//
// Fix: when GatewayCIDR is set, NewRouter must call SetTrustedProxies([GatewayCIDR])
// so Gin honors X-Forwarded-For from the trusted gateway CIDR, and c.ClientIP()
// returns the real end-user IP.
//
// These are pure HTTP-layer unit tests: no DB, no testcontainer.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/workspace/internal/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalRouterCfg builds a RouterConfig suitable for router-wiring tests.
func minimalRouterCfg(gatewayCIDR string) *handler.RouterConfig {
	return &handler.RouterConfig{
		GatewayCIDR:          gatewayCIDR,
		ContractServiceToken: "test-service-token-at-least-32-ch",
		GatewayHMACSecret:    "", // dev mode — no gateway HMAC required
	}
}

// TestNewRouter_TrustedProxies_GatewayCIDRSet proves that when GatewayCIDR is set
// Gin uses X-Forwarded-For as the client IP, allowing the rate limiter and
// signer_ip to key on the real end-user IP rather than the gateway IP.
func TestNewRouter_TrustedProxies_GatewayCIDRSet(t *testing.T) {
	r := handler.NewRouter(minimalRouterCfg("10.0.0.0/8"))

	// Simulate a request arriving from the gateway (10.1.2.3) with XFF saying
	// the real client is 203.0.113.42 (TEST-NET-3, RFC 5737).
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", http.NoBody)
	req.RemoteAddr = "10.1.2.3:54321"                 // simulated gateway peer
	req.Header.Set("X-Forwarded-For", "203.0.113.42") // real client IP the gateway forwards

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// /healthz returns 200 — the critical assertion is that the router does NOT
	// panic when GatewayCIDR is a valid CIDR, proving SetTrustedProxies succeeds.
	require.Equal(t, http.StatusOK, w.Code,
		"NewRouter with valid GatewayCIDR must serve requests without panicking")
}

// TestNewRouter_TrustedProxies_EmptyCIDR proves that when GatewayCIDR is empty
// the router falls back to SetTrustedProxies(nil) (safe dev default).
func TestNewRouter_TrustedProxies_EmptyCIDR(t *testing.T) {
	r := handler.NewRouter(minimalRouterCfg(""))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", http.NoBody)
	req.RemoteAddr = "127.0.0.1:8888"
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // ignored: no trusted proxy

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code,
		"NewRouter with empty GatewayCIDR must serve requests without panicking")
}

// TestNewRouter_TrustedProxies_InvalidCIDR_Panics proves that an invalid GatewayCIDR
// causes a panic at startup — surfacing a config bug immediately rather than running
// silently with wrong proxy trust.
func TestNewRouter_TrustedProxies_InvalidCIDR_Panics(t *testing.T) {
	cfg := &handler.RouterConfig{
		GatewayCIDR:          "not-a-cidr",
		ContractServiceToken: "test-service-token-at-least-32-ch",
	}

	assert.Panics(t, func() {
		handler.NewRouter(cfg)
	}, "NewRouter with invalid GatewayCIDR must panic to surface the config bug at boot")
}
