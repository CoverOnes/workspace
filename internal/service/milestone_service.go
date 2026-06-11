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
	tx         store.MilestoneTxManager
	publisher  events.Publisher
}

// NewMilestoneService returns a MilestoneService.
func NewMilestoneService(
	contracts store.MultipartyContractStore,
	milestones store.MilestoneStore,
	parties store.MultipartyPartyStore,
	tx store.MilestoneTxManager,
	publisher events.Publisher,
) *MilestoneService {
	return &MilestoneService{
		contracts:  contracts,
		milestones: milestones,
		parties:    parties,
		tx:         tx,
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
// Returns ErrInvalidTransition if the contract is not in ACTIVE status (DRAFT,
// PENDING_SIGNATURES, ADDENDUM_PENDING, CANCELED, COMPLETED are all rejected).
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

	// Status guard: milestones may only be added to ACTIVE contracts.
	// DRAFT / PENDING_SIGNATURES / ADDENDUM_PENDING: contract is not yet activated
	// and has no finalized, fully-signed roster. CANCELED / COMPLETED: terminal
	// states where disbursements must not be created. This mirrors the ACTIVE-only
	// guard already enforced by GetPartyRoster (line 181).
	if contract.Status != domain.MultipartyContractStatusActive {
		return nil, fmt.Errorf("%w: milestones may only be added to ACTIVE contracts (contract is %s)",
			domain.ErrInvalidTransition, contract.Status)
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
// The contract-status ACTIVE guard and the MarkCompleted write execute inside a
// single transaction with SELECT FOR UPDATE on the contract row. This prevents a
// concurrent CancelContract (ACTIVE→CANCELED) from racing between the guard and
// the write, which would complete a milestone on a CANCELED contract and fire a
// disbursement event for real money. (See backend-security-design §1 / M-race fix.)
//
// Returns ErrNotContractOwner (404) if the caller is not the poster.
// Returns ErrInvalidTransition if the contract is not in ACTIVE status.
// Returns ErrMilestoneAlreadyDone if the milestone is already COMPLETED.
// Returns ErrMilestoneNotFound if the milestone does not exist.
func (s *MilestoneService) CompleteMilestone(ctx context.Context, in CompleteMilestoneInput) (*domain.Milestone, error) {
	// Owner check against the non-locked read (fast path — callerID is immutable).
	// We re-verify ownership under the lock inside the tx for defense-in-depth.
	contract, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertContractOwner(contract, in.CallerID); err != nil {
		return nil, err
	}

	// Pre-tx milestone IDOR check: verify the milestone belongs to this contract
	// before entering the transaction (avoids holding the row lock during a separate
	// milestone table read that is not prone to the ACTIVE-cancel race).
	existing, err := s.milestones.GetByID(ctx, in.MilestoneID)
	if err != nil {
		return nil, err
	}

	if existing.MultiContractID != in.ContractID {
		// Milestone exists but belongs to a different contract — treat as not found.
		return nil, domain.ErrMilestoneNotFound
	}

	var (
		completed      *domain.Milestone
		lockedContract *domain.MultipartyContract
	)

	txErr := s.tx.WithMilestoneTx(ctx, func(
		txCtx context.Context,
		txContracts store.MultipartyContractStore,
		txMilestones store.MilestoneStore,
	) error {
		// FOR UPDATE lock: serializes CompleteMilestone with concurrent CancelContract.
		// Re-fetch the contract under lock so the status check and the milestone write
		// are atomic — no concurrent CancelContract can sneak between them.
		c, lockErr := txContracts.GetByIDForUpdate(txCtx, in.ContractID)
		if lockErr != nil {
			return lockErr
		}

		// Re-check ownership against the locked row (defense-in-depth).
		if err := assertContractOwner(c, in.CallerID); err != nil {
			return err
		}

		// Status guard under the row lock: disbursement events may only be emitted for
		// ACTIVE contracts. This guard now runs atomically with MarkCompleted so a
		// concurrent CancelContract cannot race between the check and the write.
		if c.Status != domain.MultipartyContractStatusActive {
			return fmt.Errorf("%w: milestone completion requires an ACTIVE contract (contract is %s)",
				domain.ErrInvalidTransition, c.Status)
		}

		completedAt := time.Now().UTC()

		m, markErr := txMilestones.MarkCompleted(txCtx, in.MilestoneID, completedAt)
		if markErr != nil {
			return markErr
		}

		completed = m
		// Capture the LOCKED row (post-guard) for publishCompleted so TenderID and
		// ContractID reflect the committed state. Only ContractID and TenderID are used
		// today; capturing the locked row removes future-staleness risk if more fields
		// are added to publishCompleted.
		lockedContract = c

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	// Best-effort publish OUTSIDE the transaction (post-commit).
	// context.Background() is intentional: the request context is canceled when the
	// HTTP handler returns; the publish must outlive the request.
	capturedContract := lockedContract
	capturedMilestone := completed
	//nolint:contextcheck,gosec // G118: intentional — goroutine must outlive the request context; context.Background() is the design intent
	go func() {
		s.publishCompleted(context.Background(), capturedContract, capturedMilestone)
	}()

	return completed, nil
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

	// Upper bound: numeric(14,2) max is 999999999999.99. An amount exceeding this
	// will overflow at the DB layer and produce a 500 instead of a 400.
	if amount.GreaterThan(maxNumeric14_2) {
		return fmt.Errorf("%w: amount exceeds maximum allowed value (numeric(14,2) overflow)", domain.ErrValidation)
	}

	// Reject more than 2 decimal places: would be silently truncated by the DB
	// or produce a constraint violation (500 instead of 400).
	// Exponent() returns the scale as a negative number (e.g. -3 means 3 decimal places).
	if amount.Exponent() < -2 {
		return fmt.Errorf("%w: amount must not have more than 2 decimal places", domain.ErrValidation)
	}

	if !iso4217Re.MatchString(currency) {
		return fmt.Errorf("%w: currency must be a valid ISO 4217 code (3 uppercase letters)", domain.ErrValidation)
	}

	return nil
}
