package fileclient

// isblockedip_internal_test.go — white-box tests for isBlockedIP, the shared SSRF
// predicate used by the runtime DNS-rebinding dialer. isBlockedIP is unexported, so
// these tests live in package fileclient (not fileclient_test).

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ip           string
		blockPrivate bool
		wantBlocked  bool
		wantReason   string
	}{
		// Unspecified address — the regression cases for this fix. The OS routes a
		// connect() to 0.0.0.0 / :: to localhost, so they must be blocked like loopback.
		{name: "ipv4 unspecified 0.0.0.0", ip: "0.0.0.0", blockPrivate: false, wantBlocked: true, wantReason: "unspecified address"},
		{name: "ipv6 unspecified ::", ip: "::", blockPrivate: false, wantBlocked: true, wantReason: "unspecified address"},
		{name: "unspecified blocked even with blockPrivate", ip: "0.0.0.0", blockPrivate: true, wantBlocked: true, wantReason: "unspecified address"},

		// Loopback.
		{name: "ipv4 loopback", ip: "127.0.0.1", blockPrivate: false, wantBlocked: true, wantReason: "loopback address"},
		{name: "ipv6 loopback", ip: "::1", blockPrivate: false, wantBlocked: true, wantReason: "loopback address"},

		// Link-local / metadata range.
		{name: "ipv4 link-local metadata", ip: "169.254.169.254", blockPrivate: false, wantBlocked: true, wantReason: "link-local address"},

		// RFC1918 — only blocked when blockPrivate is set.
		{name: "rfc1918 blocked when blockPrivate", ip: "10.0.0.5", blockPrivate: true, wantBlocked: true, wantReason: "private/RFC1918 address"},
		{name: "rfc1918 allowed when not blockPrivate", ip: "10.0.0.5", blockPrivate: false, wantBlocked: false, wantReason: ""},

		// Public address — must be allowed.
		{name: "public ipv4 allowed", ip: "93.184.216.34", blockPrivate: true, wantBlocked: false, wantReason: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) returned nil — bad test fixture", tt.ip)
			}

			gotBlocked, gotReason := isBlockedIP(ip, tt.blockPrivate)
			if gotBlocked != tt.wantBlocked {
				t.Errorf("isBlockedIP(%s, %v) blocked = %v, want %v", tt.ip, tt.blockPrivate, gotBlocked, tt.wantBlocked)
			}

			if gotReason != tt.wantReason {
				t.Errorf("isBlockedIP(%s, %v) reason = %q, want %q", tt.ip, tt.blockPrivate, gotReason, tt.wantReason)
			}
		})
	}
}
