package service

import (
	"context"
	"fmt"
	"unicode"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// AuditLogService handles business logic for the contract audit log.
// It enforces append-only semantics; hash computation and chain linking
// are the responsibility of the store layer.
type AuditLogService struct {
	auditLogs store.ContractAuditLogStore
}

// NewAuditLogService returns an AuditLogService.
func NewAuditLogService(auditLogs store.ContractAuditLogStore) *AuditLogService {
	return &AuditLogService{auditLogs: auditLogs}
}

// AppendInput carries the caller-supplied fields for a new audit log entry.
type AppendInput struct {
	ContractID uuid.UUID
	EventType  string
	ActorID    uuid.UUID
	Payload    map[string]any
}

// Append validates the input and delegates the atomic lock+read+compute+insert
// operation to the store layer. The store acquires the advisory lock, reads the
// chain tail, computes the hash, and inserts — all inside one transaction.
func (s *AuditLogService) Append(ctx context.Context, in *AppendInput) (*domain.ContractAuditLog, error) {
	if err := validateAppendInput(in); err != nil {
		return nil, err
	}

	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}

	entry, err := s.auditLogs.Append(ctx, &store.AuditAppendInput{
		ContractID: in.ContractID,
		EventType:  in.EventType,
		ActorID:    in.ActorID,
		Payload:    payload,
	})
	if err != nil {
		return nil, fmt.Errorf("append audit log entry: %w", err)
	}

	return entry, nil
}

// GetAuditLog returns the full audit log for a contract along with an integrity flag.
// The integrity flag is true when the chain is intact, false if any entry's hash
// does not match its recomputed value or the prev_hash linkage is broken.
func (s *AuditLogService) GetAuditLog(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAuditLog, bool, error) {
	entries, err := s.auditLogs.ListByContract(ctx, contractID)
	if err != nil {
		return nil, false, fmt.Errorf("list audit log for contract %s: %w", contractID, err)
	}

	intact, err := domain.VerifyAuditChain(entries)
	if err != nil {
		return nil, false, fmt.Errorf("verify audit chain for contract %s: %w", contractID, err)
	}

	return entries, intact, nil
}

// validateAppendInput performs client-side validation before hitting the DB.
func validateAppendInput(in *AppendInput) error {
	if in.ContractID == uuid.Nil {
		return fmt.Errorf("%w: contractId is required", domain.ErrValidation)
	}

	if in.EventType == "" {
		return fmt.Errorf("%w: eventType is required", domain.ErrValidation)
	}

	if len(in.EventType) > 100 {
		return fmt.Errorf("%w: eventType must be at most 100 characters", domain.ErrValidation)
	}

	// eventType must contain only printable ASCII letters, digits, and underscores.
	// This prevents control characters and Unicode that would differ between DB CHECK
	// and Go length counting.
	for _, r := range in.EventType {
		if !unicode.IsPrint(r) || r > 127 {
			return fmt.Errorf("%w: eventType must contain only printable ASCII characters", domain.ErrValidation)
		}
	}

	if in.ActorID == uuid.Nil {
		return fmt.Errorf("%w: actorId is required", domain.ErrValidation)
	}

	return nil
}
