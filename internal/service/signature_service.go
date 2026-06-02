package service

import (
	"context"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// SignatureService handles signature-related read operations.
type SignatureService struct {
	contracts store.ContractStore
	sigs      store.SignatureStore
}

// NewSignatureService returns a SignatureService.
func NewSignatureService(contracts store.ContractStore, sigs store.SignatureStore) *SignatureService {
	return &SignatureService{contracts: contracts, sigs: sigs}
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
