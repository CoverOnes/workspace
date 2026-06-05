package service

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// MilestoneService implements business logic for multiparty contract milestones.
// Milestone management is restricted to the contract's poster (tender owner).
// Milestone amounts are NOT required to sum to any contract total — the poster
// adds milestones incrementally; payment re-checks the sum at settlement-plan time.
type MilestoneService struct {
	contracts  store.MultipartyContractStore
	milestones store.MilestoneStore
	parties    store.MultipartyPartyStore
	publisher  events.Publisher
}

// NewMilestoneService returns a MilestoneService.
func NewMilestoneService(
	contracts store.MultipartyContractStore,
	milestones store.MilestoneStore,
	parties store.MultipartyPartyStore,
	publisher events.Publisher,
) *MilestoneService {
	return &MilestoneService{
		contracts:  contracts,
		milestones: milestones,
		parties:    parties,
		publisher:  publisher,
	}
}

// AddMilestoneInput carries validated input for milestone creation.
type AddMilestoneInput struct {
	ContractID uuid.UUID
	CallerID   uuid.UUID // must equal contract.PosterUserID
	Name       string
	Amount     decimal.Decimal
	Currency   string
	Sequence   int
}

// AddMilestone adds a new PENDING milestone to an ACTIVE multiparty contract.
// Only the contract's poster (PosterUserID) may call this endpoint.
// Returns ErrNotContractOwner (mapped to 404) if the caller is not the poster.
// Returns ErrMultipartyContractNotFound if the contract does not exist.
func (s *MilestoneService) AddMilestone(ctx context.Context, in *AddMilestoneInput) (*domain.Milestone, error) {
	if err := validateMilestoneInput(in.Name, in.Amount, in.Currency); err != nil {
		return nil, err
	}

	contract, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertContractOwner(contract, in.CallerID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	m := &domain.Milestone{
		ID:              uuid.New(),
		MultiContractID: in.ContractID,
		Name:            in.Name,
		Amount:          in.Amount,
		Currency:        in.Currency,
		Sequence:        in.Sequence,
		Status:          domain.MilestoneStatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := s.milestones.Create(ctx, m); err != nil {
		return nil, fmt.Errorf("create milestone: %w", err)
	}

	return m, nil
}

// ListMilestones returns all milestones for a multiparty contract.
// Only the contract's poster may list milestones (same IDOR guard as Add/Complete).
func (s *MilestoneService) ListMilestones(ctx context.Context, contractID, callerID uuid.UUID) ([]*domain.Milestone, error) {
	contract, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	if err := assertContractOwner(contract, callerID); err != nil {
		return nil, err
	}

	ms, err := s.milestones.ListByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list milestones: %w", err)
	}

	if ms == nil {
		ms = []*domain.Milestone{}
	}

	return ms, nil
}

// CompleteMilestoneInput carries validated input for milestone completion.
type CompleteMilestoneInput struct {
	ContractID  uuid.UUID
	MilestoneID uuid.UUID
	CallerID    uuid.UUID // must equal contract.PosterUserID
}

// CompleteMilestone marks a PENDING milestone as COMPLETED and publishes a
// workspace.contract_completed event (§14 dotted-lowercase channel).
// Best-effort publish: the milestone row is committed first; a publish failure
// is logged as a warning but does NOT roll back the completion.
//
// Returns ErrNotContractOwner (404) if the caller is not the poster.
// Returns ErrMilestoneAlreadyDone if the milestone is already COMPLETED.
// Returns ErrMilestoneNotFound if the milestone does not exist.
func (s *MilestoneService) CompleteMilestone(ctx context.Context, in CompleteMilestoneInput) (*domain.Milestone, error) {
	contract, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertContractOwner(contract, in.CallerID); err != nil {
		return nil, err
	}

	// Verify the milestone belongs to this contract before completing it (IDOR guard).
	existing, err := s.milestones.GetByID(ctx, in.MilestoneID)
	if err != nil {
		return nil, err
	}

	if existing.MultiContractID != in.ContractID {
		// Milestone exists but belongs to a different contract — treat as not found.
		return nil, domain.ErrMilestoneNotFound
	}

	completedAt := time.Now().UTC()

	m, err := s.milestones.MarkCompleted(ctx, in.MilestoneID, completedAt)
	if err != nil {
		return nil, err
	}

	// Best-effort publish: log on failure, never fail the caller.
	s.publishCompleted(ctx, contract, m)

	return m, nil
}

// GetPartyRoster returns the frozen ACTIVE-party roster for a multiparty contract.
// This is the S2S roster endpoint consumed by payment at settlement-plan creation.
// No owner check — this is called by payment service using X-Service-Token, not end users.
// Returns ErrMultipartyContractNotFound (mapped to 404) if the contract does not exist.
//
// Status guard: only ACTIVE and COMPLETED contracts have a stable, fully-allocated
// roster (Σ share_bps == 10000, no mid-reallocation). ADDENDUM_PENDING, PENDING_SIGNATURES,
// DRAFT, and CANCELED are transient or pre-activation states where the roster may be
// inconsistent. Payment must only settle against a finalized roster.
func (s *MilestoneService) GetPartyRoster(ctx context.Context, contractID uuid.UUID) ([]*domain.MultipartyContractParty, error) {
	// Existence check: 404 on miss (phantoms must not return a successful empty roster).
	contract, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	// Status guard: reject transient states where shares may be mid-reallocation.
	if contract.Status != domain.MultipartyContractStatusActive &&
		contract.Status != domain.MultipartyContractStatusCompleted {
		return nil, fmt.Errorf("%w: roster not stable while contract is %s",
			domain.ErrInvalidTransition, contract.Status)
	}

	parties, err := s.parties.ListActiveByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list active parties: %w", err)
	}

	if parties == nil {
		parties = []*domain.MultipartyContractParty{}
	}

	return parties, nil
}

// publishCompleted publishes the workspace.contract_completed event.
// Best-effort: logs a warning on failure, does not propagate the error.
func (s *MilestoneService) publishCompleted(ctx context.Context, contract *domain.MultipartyContract, m *domain.Milestone) {
	evt := &domain.MultipartyContractCompletedEvent{
		EventID:    uuid.New(),
		OccurredAt: time.Now().UTC(),
		Version:    1,
	}
	evt.Data.ContractID = contract.ID
	evt.Data.TenderID = contract.TenderID
	evt.Data.MilestoneID = m.ID
	evt.Data.Amount = m.Amount
	evt.Data.Currency = m.Currency

	if pubErr := s.publisher.PublishMultipartyContractCompleted(ctx, evt); pubErr != nil {
		slog.Warn(
			"publish contract_completed event failed",
			"contract_id", contract.ID,
			"milestone_id", m.ID,
			"err", pubErr,
		)
	}
}

// assertContractOwner returns ErrNotContractOwner if the caller is not the contract poster.
// Poster-ownership check: the contract must have a PosterUserID set and it must match callerID.
// Contracts created before migration 000007 (PosterUserID == nil) always fail this check.
func assertContractOwner(contract *domain.MultipartyContract, callerID uuid.UUID) error {
	if contract.PosterUserID == nil || *contract.PosterUserID != callerID {
		return domain.ErrNotContractOwner
	}

	return nil
}

// iso4217Re matches a valid ISO 4217 currency code: exactly 3 uppercase ASCII letters.
var iso4217Re = regexp.MustCompile(`^[A-Z]{3}$`)

// validateMilestoneInput validates milestone creation fields.
func validateMilestoneInput(name string, amount decimal.Decimal, currency string) error {
	runes := []rune(name)
	if len(runes) == 0 || len(runes) > 255 {
		return fmt.Errorf("%w: name must be 1-255 characters", domain.ErrValidation)
	}

	// Reject control characters in name (null byte, CR, LF, and runes < 0x20 except tab).
	for _, r := range runes {
		if r == '\x00' || r == '\r' || r == '\n' || (r < 0x20 && r != '\t') {
			return fmt.Errorf("%w: name must not contain control characters", domain.ErrValidation)
		}
	}

	if amount.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("%w: amount must be greater than 0", domain.ErrValidation)
	}

	if !iso4217Re.MatchString(currency) {
		return fmt.Errorf("%w: currency must be a valid ISO 4217 code (3 uppercase letters)", domain.ErrValidation)
	}

	return nil
}
