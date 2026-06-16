package client_test

// Unit tests for HTTPFileClient using httptest.Server to simulate the file service.
// These tests cover: happy path, non-2xx error mapping, body size cap,
// malformed JSON response, and X-Service-Token header verification.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CoverOnes/workspace/internal/client"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testServiceToken = "test-token-value-for-unit-tests-abc123"
	jsonDataKey      = "data" // JSON envelope key used in file service responses
)

// buildTestClient returns an HTTPFileClient pointing at the given server URL.
func buildTestClient(serverURL string) *client.HTTPFileClient {
	return client.NewHTTPFileClient(serverURL, testServiceToken, &http.Client{})
}

// --- StoreSystemFile tests ---

func TestHTTPFileClient_StoreSystemFile_HappyPath(t *testing.T) {
	expectedID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	expectedKey := "proofs/aaaaaaaa.pdf"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/internal/v1/files", r.URL.Path)
		assert.Equal(t, testServiceToken, r.Header.Get("X-Service-Token"),
			"X-Service-Token header must be set on store request")
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonDataKey: map[string]string{
				"fileId":    expectedID.String(),
				"objectKey": expectedKey,
			},
		})
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)

	result, err := c.StoreSystemFile(t.Context(), client.StoreSystemFileInput{
		ContentType:   "application/pdf",
		Filename:      "contract-proof.pdf",
		Data:          []byte("%PDF-1.4 fake"),
		SystemContext: "contract-proof",
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, expectedID, result.FileID)
	assert.Equal(t, expectedKey, result.ObjectKey)
}

func TestHTTPFileClient_StoreSystemFile_Non2xxError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"bad_request", http.StatusBadRequest},
		{"unauthorized", http.StatusUnauthorized},
		{"internal_server_error", http.StatusInternalServerError},
		{"service_unavailable", http.StatusServiceUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"internal"}`, tc.statusCode)
			}))
			defer srv.Close()

			c := buildTestClient(srv.URL)
			_, err := c.StoreSystemFile(t.Context(), client.StoreSystemFileInput{
				Data: []byte("pdf"),
			})

			require.Error(t, err, "non-2xx status %d must return an error", tc.statusCode)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tc.statusCode),
				"error message must include the HTTP status code")
			// Must NOT leak the server's internal error body.
			assert.NotContains(t, err.Error(), `"error":"internal"`,
				"error must not leak internal service response body")
		})
	}
}

func TestHTTPFileClient_StoreSystemFile_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	_, err := c.StoreSystemFile(t.Context(), client.StoreSystemFileInput{Data: []byte("pdf")})

	require.Error(t, err, "malformed JSON response must return an error")
}

func TestHTTPFileClient_StoreSystemFile_InvalidFileIDInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"fileId":"not-a-uuid","objectKey":"key"}}`))
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	_, err := c.StoreSystemFile(t.Context(), client.StoreSystemFileInput{Data: []byte("pdf")})

	require.Error(t, err, "invalid UUID in fileId must return an error")
	assert.Contains(t, err.Error(), "file id")
}

func TestHTTPFileClient_StoreSystemFile_TokenInHeaderNotURL(t *testing.T) {
	// Verify the token is never appended to the request URL (only in header).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Token must NOT appear in the raw URL query string.
		assert.NotContains(t, r.URL.RawQuery, testServiceToken,
			"service token must not appear in URL query string")
		assert.Equal(t, testServiceToken, r.Header.Get("X-Service-Token"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonDataKey: map[string]string{
				"fileId":    uuid.NewString(),
				"objectKey": "k",
			},
		})
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	_, err := c.StoreSystemFile(t.Context(), client.StoreSystemFileInput{Data: []byte("pdf")})

	require.NoError(t, err)
}

// --- PresignDownload tests ---

func TestHTTPFileClient_PresignDownload_HappyPath(t *testing.T) {
	fileID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	expectedURL := "https://storage.example.com/proof.pdf?sig=abc"
	expectedTTL := 300

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/internal/v1/files/"+fileID.String()+"/download-url", r.URL.Path)
		assert.Equal(t, testServiceToken, r.Header.Get("X-Service-Token"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonDataKey: map[string]interface{}{
				"url":        expectedURL,
				"ttlSeconds": expectedTTL,
			},
		})
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	gotURL, gotTTL, err := c.PresignDownload(t.Context(), fileID)

	require.NoError(t, err)
	assert.Equal(t, expectedURL, gotURL)
	assert.Equal(t, expectedTTL, gotTTL)
}

func TestHTTPFileClient_PresignDownload_Non2xxError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"not_found", http.StatusNotFound},
		{"forbidden", http.StatusForbidden},
		{"internal_error", http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "internal error details", tc.statusCode)
			}))
			defer srv.Close()

			c := buildTestClient(srv.URL)
			_, _, err := c.PresignDownload(t.Context(), uuid.New())

			require.Error(t, err, "non-2xx %d must return an error", tc.statusCode)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tc.statusCode))
			// Must not leak internal body.
			assert.NotContains(t, err.Error(), "internal error details")
		})
	}
}

func TestHTTPFileClient_PresignDownload_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{invalid"))
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	_, _, err := c.PresignDownload(t.Context(), uuid.New())

	require.Error(t, err, "malformed JSON must return an error")
}

func TestHTTPFileClient_PresignDownload_TokenInHeaderNotURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotContains(t, r.URL.RawQuery, testServiceToken,
			"service token must not appear in URL query string for presign request")
		assert.Equal(t, testServiceToken, r.Header.Get("X-Service-Token"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonDataKey: map[string]interface{}{
				"url":        "https://example.com/obj",
				"ttlSeconds": 60,
			},
		})
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	_, _, err := c.PresignDownload(t.Context(), uuid.New())

	require.NoError(t, err)
}

// --- Body size cap test ---

func TestHTTPFileClient_StoreSystemFile_LargeErrorBody_NotPanics(t *testing.T) {
	// Simulate a server that returns a very large body on a 500 error.
	// The client must read at most maxResponseBodyBytes without OOM.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Write 2 MB of garbage (exceeds the 1 MB cap).
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("X", 2<<20)))
	}))
	defer srv.Close()

	c := buildTestClient(srv.URL)
	_, err := c.StoreSystemFile(t.Context(), client.StoreSystemFileInput{Data: []byte("pdf")})

	// Must return an error (non-2xx) without panicking or hanging.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// --- FakeFileClient tests ---

func TestFakeFileClient_StoreAndRetrieve(t *testing.T) {
	fc := client.NewFakeFileClient(nil)

	in := client.StoreSystemFileInput{
		ContentType:   "application/pdf",
		Filename:      "proof.pdf",
		Data:          []byte("%PDF-1.4"),
		SystemContext: "test",
	}

	result, err := fc.StoreSystemFile(t.Context(), in)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEqual(t, uuid.Nil, result.FileID)
	assert.NotEmpty(t, result.ObjectKey)

	// Get returns the stored bytes.
	stored := fc.Get(result.FileID)
	assert.Equal(t, in.Data, stored)
}

func TestFakeFileClient_PresignDownload_DefaultURL(t *testing.T) {
	fc := client.NewFakeFileClient(nil)
	result, err := fc.StoreSystemFile(t.Context(), client.StoreSystemFileInput{
		Data: []byte("pdf"),
	})
	require.NoError(t, err)

	url, ttl, err := fc.PresignDownload(t.Context(), result.FileID)

	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Equal(t, 300, ttl, "default presign TTL must be 300s")
	assert.Contains(t, url, result.FileID.String())
}

func TestFakeFileClient_PresignDownload_UnknownID(t *testing.T) {
	fc := client.NewFakeFileClient(nil)

	_, _, err := fc.PresignDownload(t.Context(), uuid.New())

	require.Error(t, err, "PresignDownload for unknown ID must return an error")
}

func TestFakeFileClient_CustomPresign(t *testing.T) {
	customURL := "https://custom.example.com/download"
	customTTL := 60

	fc := client.NewFakeFileClient(func(id uuid.UUID) (string, int) {
		return customURL + "/" + id.String(), customTTL
	})

	result, err := fc.StoreSystemFile(t.Context(), client.StoreSystemFileInput{Data: []byte("x")})
	require.NoError(t, err)

	url, ttl, err := fc.PresignDownload(t.Context(), result.FileID)

	require.NoError(t, err)
	assert.Equal(t, customURL+"/"+result.FileID.String(), url)
	assert.Equal(t, customTTL, ttl)
}

func TestFakeFileClient_StoredIDs_ReturnsAll(t *testing.T) {
	fc := client.NewFakeFileClient(nil)

	// Store 3 files.
	ids := make(map[uuid.UUID]bool)
	for i := 0; i < 3; i++ {
		r, err := fc.StoreSystemFile(t.Context(), client.StoreSystemFileInput{
			Data: []byte(fmt.Sprintf("pdf-%d", i)),
		})
		require.NoError(t, err)
		ids[r.FileID] = true
	}

	storedIDs := fc.StoredIDs()
	assert.Len(t, storedIDs, 3)
	for _, id := range storedIDs {
		assert.True(t, ids[id], "StoredIDs must contain only the IDs that were stored")
	}
}
