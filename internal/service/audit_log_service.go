package service

import (
	"context"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// AuditLogService handles business logic for the contract audit log.
// It enforces append-only semantics and computes the hash chain automatically.
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

// Append inserts a new entry into the hash chain for the given contract.
// It fetches the latest entry for the contract, computes prev_hash and hash,
// then delegates the insert (with advisory lock) to the store.
func (s *AuditLogService) Append(ctx context.Context, in *AppendInput) (*domain.ContractAuditLog, error) {
	if err := validateAppendInput(in); err != nil {
		return nil, err
	}

	// Fetch the current tail to determine prev_hash.
	// The advisory lock in the store layer ensures that between this read and the
	// INSERT, no other goroutine can insert for the same contract_id.
	prevHash, err := s.latestHash(ctx, in.ContractID)
	if err != nil {
		return nil, fmt.Errorf("fetch latest audit hash for contract %s: %w", in.ContractID, err)
	}

	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}

	hash, err := domain.AuditEntryDigest(prevHash, in.ContractID, in.EventType, in.ActorID, payload)
	if err != nil {
		return nil, fmt.Errorf("compute audit entry digest: %w", err)
	}

	entry := &domain.ContractAuditLog{
		ID:         uuid.New(),
		ContractID: in.ContractID,
		EventType:  in.EventType,
		ActorID:    in.ActorID,
		Payload:    payload,
		PrevHash:   prevHash,
		Hash:       hash,
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.auditLogs.Append(ctx, entry); err != nil {
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

// latestHash returns the hash of the most recent entry for the contract,
// or the empty string if no entries exist yet (genesis entry).
func (s *AuditLogService) latestHash(ctx context.Context, contractID uuid.UUID) (string, error) {
	entries, err := s.auditLogs.ListByContract(ctx, contractID)
	if err != nil {
		return "", err
	}

	if len(entries) == 0 {
		return "", nil
	}

	return entries[len(entries)-1].Hash, nil
}

func validateAppendInput(in *AppendInput) error {
	if in.ContractID == uuid.Nil {
		return fmt.Errorf("%w: contractId is required", domain.ErrValidation)
	}

	if in.EventType == "" {
		return fmt.Errorf("%w: eventType is required", domain.ErrValidation)
	}

	if in.ActorID == uuid.Nil {
		return fmt.Errorf("%w: actorId is required", domain.ErrValidation)
	}

	return nil
}
