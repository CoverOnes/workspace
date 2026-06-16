// Package client provides service-to-service HTTP client implementations.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FileClient abstracts the file service S2S API for storing and retrieving files.
type FileClient interface {
	// StoreSystemFile stores a system-generated (non-user) file via the file service S2S API.
	// Returns StoreSystemFileResult containing the assigned FileID and ObjectKey on success.
	StoreSystemFile(ctx context.Context, in StoreSystemFileInput) (*StoreSystemFileResult, error)
	// PresignDownload returns a single-use, short-lived presigned download URL for a stored file.
	// Returns the URL string, its TTL in seconds, and any transport/service error.
	PresignDownload(ctx context.Context, fileID uuid.UUID) (url string, ttlSeconds int, err error)
}

// StoreSystemFileInput carries the parameters for storing a system-generated file.
type StoreSystemFileInput struct {
	// ContentType is the MIME type (e.g. "application/pdf").
	ContentType string
	// Filename is the suggested filename for the stored object.
	Filename string
	// Data is the raw file bytes to store.
	Data []byte
	// SystemContext is a free-form label describing what generated this file
	// (e.g. "contract-proof"). Stored as metadata by the file service.
	SystemContext string
}

// StoreSystemFileResult is returned by StoreSystemFile on success.
type StoreSystemFileResult struct {
	// FileID is the UUID assigned by the file service to the stored object.
	FileID uuid.UUID
	// ObjectKey is the storage layer object key (e.g. S3 key or MinIO path).
	ObjectKey string
}

// maxResponseBodyBytes caps the response body read from the file service at 1 MB.
// Prevents an unbounded allocation if the file service returns an oversized error body.
const maxResponseBodyBytes = 1 << 20 // 1 MB

// HTTPFileClient is the production FileClient backed by the file service HTTP API.
// Base URL: cfg.FileServiceBaseURL. Auth: X-Service-Token header (never in URL).
type HTTPFileClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewHTTPFileClient returns an HTTPFileClient.
//
//   - baseURL: the file service base URL (e.g. "https://file-svc.internal").
//   - token: the S2S service token sent in X-Service-Token (never logged).
//   - httpClient: a pre-configured *http.Client with a sane timeout; if nil,
//     a 10-second default client is used.
func NewHTTPFileClient(baseURL, token string, httpClient *http.Client) *HTTPFileClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	return &HTTPFileClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    httpClient,
	}
}

// storeFileRequest is the JSON body sent to the file service store endpoint.
type storeFileRequest struct {
	ContentType   string `json:"contentType"`
	Filename      string `json:"filename"`
	Data          []byte `json:"data"` // base64-encoded by encoding/json
	SystemContext string `json:"systemContext"`
}

// storeFileResponseData is the "data" field in the file service store response envelope.
type storeFileResponseData struct {
	FileID    string `json:"fileId"`
	ObjectKey string `json:"objectKey"`
}

// presignResponseData is the "data" field in the file service presign response envelope.
type presignResponseData struct {
	URL        string `json:"url"`
	TTLSeconds int    `json:"ttlSeconds"`
}

// StoreSystemFile posts to <base>/internal/v1/files with X-Service-Token authentication.
// Parses the CONVENTIONS envelope { "data": ... }. Maps non-2xx to wrapped errors.
// Request body is capped at maxResponseBodyBytes on read.
func (c *HTTPFileClient) StoreSystemFile(ctx context.Context, in StoreSystemFileInput) (*StoreSystemFileResult, error) {
	reqBody := storeFileRequest(in)

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal store file request: %w", err)
	}

	reqURL := c.baseURL + "/internal/v1/files"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("build store file request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	// X-Service-Token sent in header, never in URL (prevents log leakage).
	req.Header.Set("X-Service-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store file request: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close; error from Do takes precedence

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read store file response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Return a sanitized error that does not leak internal service details.
		return nil, fmt.Errorf("file service store returned %d", resp.StatusCode)
	}

	var envelope struct {
		Data storeFileResponseData `json:"data"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("parse store file response: %w", err)
	}

	fileID, err := uuid.Parse(envelope.Data.FileID)
	if err != nil {
		return nil, fmt.Errorf("parse file id from response %q: %w", envelope.Data.FileID, err)
	}

	return &StoreSystemFileResult{
		FileID:    fileID,
		ObjectKey: envelope.Data.ObjectKey,
	}, nil
}

// PresignDownload calls <base>/internal/v1/files/<id>/download-url to obtain a
// single-use presigned download URL. Returns url, ttlSeconds, or an error.
// Non-2xx responses are mapped to wrapped errors without leaking service internals.
func (c *HTTPFileClient) PresignDownload(ctx context.Context, fileID uuid.UUID) (presignURL string, ttlSeconds int, err error) {
	// url.PathEscape ensures the UUID (already safe) is properly encoded.
	reqURL := c.baseURL + "/internal/v1/files/" + url.PathEscape(fileID.String()) + "/download-url"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return "", 0, fmt.Errorf("build presign request: %w", err)
	}

	// X-Service-Token sent in header, never in URL.
	req.Header.Set("X-Service-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("presign request: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return "", 0, fmt.Errorf("read presign response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, fmt.Errorf("file service presign returned %d", resp.StatusCode)
	}

	var envelope struct {
		Data presignResponseData `json:"data"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", 0, fmt.Errorf("parse presign response: %w", err)
	}

	return envelope.Data.URL, envelope.Data.TTLSeconds, nil
}
