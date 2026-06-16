package fileclient

// redirect_internal_test.go — white-box tests for checkRedirectStripToken, the
// CheckRedirect hook that prevents the S2S token from leaking across a cross-origin
// HTTP redirect (Go strips Authorization/Cookie on host change but NOT custom headers).

import (
	"net/http"
	"testing"
)

func newRedirReq(t *testing.T, rawURL string, withToken bool) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		t.Fatalf("build request for %q: %v", rawURL, err)
	}

	if withToken {
		req.Header.Set("X-Service-Token", "secret-token")
	}

	return req
}

func TestCheckRedirectStripToken_CrossOrigin_StripsToken(t *testing.T) {
	orig := newRedirReq(t, "https://file.coverones.internal/v1/files", false)
	next := newRedirReq(t, "https://attacker.example.com/evil", true)

	if err := checkRedirectStripToken(next, []*http.Request{orig}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := next.Header.Get("X-Service-Token"); got != "" {
		t.Fatalf("token must be stripped on cross-origin redirect, got %q", got)
	}
}

func TestCheckRedirectStripToken_SameOrigin_KeepsToken(t *testing.T) {
	orig := newRedirReq(t, "https://file.coverones.internal/v1/files", false)
	next := newRedirReq(t, "https://file.coverones.internal/v1/files/123", true)

	if err := checkRedirectStripToken(next, []*http.Request{orig}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := next.Header.Get("X-Service-Token"); got != "secret-token" {
		t.Fatalf("token must be preserved on same-origin redirect, got %q", got)
	}
}

func TestCheckRedirectStripToken_TooManyRedirects_Errors(t *testing.T) {
	next := newRedirReq(t, "https://file.coverones.internal/v1/files", false)

	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = newRedirReq(t, "https://file.coverones.internal/v1/files", false)
	}

	if err := checkRedirectStripToken(next, via); err == nil {
		t.Fatalf("expected error after %d redirects, got nil", maxRedirects)
	}
}
