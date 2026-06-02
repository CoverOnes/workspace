package service

import (
	"context"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// WorklogService handles worklog business logic.
type WorklogService struct {
	contracts store.ContractStore
	worklogs  store.WorklogStore
}

// NewWorklogService returns a WorklogService.
func NewWorklogService(contracts store.ContractStore, worklogs store.WorklogStore) *WorklogService {
	return &WorklogService{contracts: contracts, worklogs: worklogs}
}

// CreateWorklogInput carries validated input for creating a worklog entry.
type CreateWorklogInput struct {
	ContractID  uuid.UUID
	UserID      uuid.UUID // set from X-User-Id; must be a party
	Description string
	Minutes     int
	LoggedAt    *time.Time // optional; defaults to now()
}

// CreateWorklog creates a worklog entry under a contract.
// user_id is set from X-User-Id and must be a contract party.
func (s *WorklogService) CreateWorklog(ctx context.Context, in CreateWorklogInput) (*domain.Worklog, error) {
	c, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, in.UserID); err != nil {
		return nil, err
	}

	if err := validateDescription(in.Description); err != nil {
		return nil, err
	}

	if in.Minutes <= 0 || in.Minutes > 1440 {
		return nil, fmt.Errorf("%w: minutes must be between 1 and 1440", domain.ErrValidation)
	}

	now := time.Now().UTC()
	loggedAt := now

	if in.LoggedAt != nil {
		loggedAt = in.LoggedAt.UTC()
	}

	w := &domain.Worklog{
		ID:          uuid.New(),
		ContractID:  in.ContractID,
		UserID:      in.UserID,
		Description: in.Description,
		Minutes:     in.Minutes,
		LoggedAt:    loggedAt,
		CreatedAt:   now,
	}

	if err := s.worklogs.Create(ctx, w); err != nil {
		return nil, fmt.Errorf("create worklog: %w", err)
	}

	return w, nil
}

// ListWorklogs returns all worklogs for a contract. Caller must be a party.
func (s *WorklogService) ListWorklogs(ctx context.Context, contractID, callerID uuid.UUID) ([]*domain.Worklog, error) {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, callerID); err != nil {
		return nil, err
	}

	worklogs, err := s.worklogs.ListByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list worklogs: %w", err)
	}

	return worklogs, nil
}

// DeleteWorklog soft-deletes a worklog. Author-only (user_id must equal callerID).
func (s *WorklogService) DeleteWorklog(ctx context.Context, contractID, worklogID, callerID uuid.UUID) error {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return err
	}

	if err := assertParty(c, callerID); err != nil {
		return err
	}

	w, err := s.worklogs.GetByID(ctx, worklogID)
	if err != nil {
		return err
	}

	// Cross-contract child ID smuggling prevention.
	if w.ContractID != contractID {
		return domain.ErrWorklogNotFound
	}

	// Author-only: only the creator may soft-delete their own worklog.
	if w.UserID != callerID {
		return domain.ErrWorklogNotFound // 404 not 403 (IDOR consistent)
	}

	return s.worklogs.SoftDelete(ctx, worklogID)
}
