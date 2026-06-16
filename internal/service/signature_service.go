package service

import (
	"context"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/fileclient"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// FileRegistrar is the subset of fileclient.Client used by SignatureService.
// Extracted to an interface so unit tests can stub without an HTTP server.
type FileRegistrar interface {
	Register(ctx context.Context, ownerUserID, fileID, signatureID uuid.UUID) error
	Presign(ctx context.Context, fileID, signatureID uuid.UUID) (*fileclient.PresignResponse, error)
}

// SignatureService handles signature-related read operations.
type SignatureService struct {
	contracts store.ContractStore
	sigs      store.SignatureStore
	files     FileRegistrar // may be nil when file service is not configured
}

// NewSignatureService returns a SignatureService.
// files may be nil when the file service is not configured (dev environments).
func NewSignatureService(contracts store.ContractStore, sigs store.SignatureStore, files FileRegistrar) *SignatureService {
	return &SignatureService{contracts: contracts, sigs: sigs, files: files}
}

// ListSignatures returns all signatures for a contract. Caller must be a party.
func (s *SignatureService) ListSignatures(ctx context.Context, contractID, callerID uuid.UUID) ([]*domain.Signature, error) {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, callerID); err != nil {
		return nil, err
	}

	sigs, err := s.sigs.ListByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list signatures: %w", err)
	}

	return sigs, nil
}

// DownloadURLInput carries validated input for the attachment download-url endpoint.
type DownloadURLInput struct {
	ContractID  uuid.UUID
	SignatureID uuid.UUID
	CallerID    uuid.UUID
}

// DownloadURLResult carries the presigned URL response.
type DownloadURLResult struct {
	URL        string `json:"url"`
	TTLSeconds int    `json:"ttlSeconds"`
}

// GetAttachmentDownloadURL enforces the party gate and returns a presigned download URL
// for the signature's attached document.
//
// Returns:
//   - ErrNotFound if contract or signature does not exist, or caller is not a party.
//   - ErrNotFound if the signature has no file_id (no attachment).
//   - error wrapping fileclient error if the presign call fails.
func (s *SignatureService) GetAttachmentDownloadURL(ctx context.Context, in DownloadURLInput) (*DownloadURLResult, error) {
	// Load contract and enforce party gate (IDOR-safe: 404 for non-parties).
	c, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, in.CallerID); err != nil {
		return nil, err
	}

	// Load the specific signature.
	sig, err := s.sigs.GetByID(ctx, in.SignatureID)
	if err != nil {
		return nil, err
	}

	// Verify the signature belongs to this contract (prevent cross-contract IDOR).
	if sig.ContractID != in.ContractID {
		return nil, domain.ErrNotFound
	}

	// No attachment on this signature.
	if sig.FileID == nil {
		return nil, domain.ErrNotFound
	}

	if s.files == nil {
		return nil, fmt.Errorf("fileclient not configured")
	}

	resp, err := s.files.Presign(ctx, *sig.FileID, sig.ID)
	if err != nil {
		return nil, fmt.Errorf("presign attachment: %w", err)
	}

	return &DownloadURLResult{
		URL:        resp.URL,
		TTLSeconds: resp.TTLSeconds,
	}, nil
}
