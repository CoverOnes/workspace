// Package fileclient provides a typed S2S client for the CoverOnes file service.
//
// Security posture (backend-security-design §4):
//   - Token is NEVER logged or included in URLs.
//   - Token is transmitted only via X-Service-Token request header.
//   - HTTP client timeout is enforced so callers cannot block indefinitely.
package fileclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// defaultHTTPTimeout caps individual S2S calls to the file service.
	defaultHTTPTimeout = 10 * time.Second

	// maxResponseBytes guards against unexpectedly large file-service responses.
	maxResponseBytes = 64 * 1024 // 64 KiB

	// entityTypeSignature is the entity_type sent to the file service for signature attachments.
	entityTypeSignature = "signature"
)

// Client is an S2S HTTP client for the CoverOnes file service.
type Client struct {
	base      string       // e.g. "https://file.coverones.internal"
	serviceID string       // X-Service-Id header value (default "workspace")
	token     string       // X-Service-Token header value — NEVER logged
	http      *http.Client // configurable for testing
}

// Config carries the configuration for a Client.
type Config struct {
	// BaseURL is the base URL of the file service (e.g. "https://file.coverones.internal").
	// Required.
	BaseURL string
	// ServiceID is the value sent in X-Service-Id (default: "workspace").
	ServiceID string
	// Token is the pre-shared S2S token sent in X-Service-Token. Required.
	// NEVER included in URL query strings or log output.
	Token string
}

// New returns a Client configured from cfg.
// Callers should validate cfg with config.Config.validate() before calling New.
func New(cfg Config) *Client {
	sid := cfg.ServiceID
	if sid == "" {
		sid = "workspace"
	}

	return &Client{
		base:      cfg.BaseURL,
		serviceID: sid,
		token:     cfg.Token,
		http:      &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// registerRequest is the JSON body for POST /internal/v1/attachments.
type registerRequest struct {
	FileID     uuid.UUID `json:"fileId"`
	EntityType string    `json:"entityType"`
	EntityID   uuid.UUID `json:"entityId"`
}

// presignRequest is the JSON body for POST /internal/v1/attachments/presign.
type presignRequest struct {
	FileID     uuid.UUID `json:"fileId"`
	EntityType string    `json:"entityType"`
	EntityID   uuid.UUID `json:"entityId"`
}

// PresignResponse is the response body from POST /internal/v1/attachments/presign.
type PresignResponse struct {
	URL        string `json:"url"`
	TTLSeconds int    `json:"ttlSeconds"`
}

// Register registers an uploaded file as an attachment in the file service.
//
// ownerUserID is set as X-User-Id (the signer who owns the file).
// fileID is the file's UUID already present in the file service.
// signatureID is the workspace-side signature being attached.
//
// Returns nil on 204; non-nil on any error (network, HTTP, or unexpected status).
func (c *Client) Register(ctx context.Context, ownerUserID, fileID, signatureID uuid.UUID) error {
	body, err := json.Marshal(registerRequest{
		FileID:     fileID,
		EntityType: entityTypeSignature,
		EntityID:   signatureID,
	})
	if err != nil {
		return fmt.Errorf("fileclient register: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.base+"/internal/v1/attachments",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("fileclient register: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Id", c.serviceID)
	req.Header.Set("X-Service-Token", c.token) // token in header, never in URL
	req.Header.Set("X-User-Id", ownerUserID.String())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fileclient register: request: %w", err)
	}

	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("fileclient register: close response body", "err", closeErr)
		}
	}()

	// Drain body to allow connection re-use; limit to avoid DoS.
	if _, drainErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes)); drainErr != nil {
		slog.Warn("fileclient register: drain response body", "err", drainErr)
	}

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("fileclient register: unexpected status %d", resp.StatusCode)
	}

	return nil
}

// Presign requests a time-limited download URL from the file service.
//
// fileID must match the file_id stored on the signature row.
// signatureID is the workspace-side signature owning this attachment.
//
// Returns a PresignResponse on 200; non-nil error otherwise.
func (c *Client) Presign(ctx context.Context, fileID, signatureID uuid.UUID) (*PresignResponse, error) {
	body, err := json.Marshal(presignRequest{
		FileID:     fileID,
		EntityType: entityTypeSignature,
		EntityID:   signatureID,
	})
	if err != nil {
		return nil, fmt.Errorf("fileclient presign: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.base+"/internal/v1/attachments/presign",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("fileclient presign: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Id", c.serviceID)
	req.Header.Set("X-Service-Token", c.token) // token in header, never in URL

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fileclient presign: request: %w", err)
	}

	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("fileclient presign: close response body", "err", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		// Drain before returning error so connection can be reused.
		if _, drainErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes)); drainErr != nil {
			slog.Warn("fileclient presign: drain error response body", "err", drainErr)
		}

		return nil, fmt.Errorf("fileclient presign: unexpected status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes)

	var result PresignResponse

	if decodeErr := json.NewDecoder(limited).Decode(&result); decodeErr != nil {
		return nil, fmt.Errorf("fileclient presign: decode response: %w", decodeErr)
	}

	// Validate the returned URL uses HTTPS. A non-HTTPS URL indicates a misconfigured
	// or compromised file service and must not be returned to callers.
	if !strings.HasPrefix(result.URL, "https://") {
		return nil, fmt.Errorf("fileclient presign: response URL is not HTTPS (got %q); rejecting", result.URL)
	}

	return &result, nil
}
