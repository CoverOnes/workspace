package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ContractService handles contract business logic.
type ContractService struct {
	contracts store.ContractStore
	sigs      store.SignatureStore
	tx        store.TxManager
	publisher events.Publisher
	proofGen  ProofGenerator // optional; nil = skip proof generation
}

// NewContractService returns a ContractService.
func NewContractService(
	contracts store.ContractStore,
	sigs store.SignatureStore,
	tx store.TxManager,
	publisher events.Publisher,
) *ContractService {
	return &ContractService{
		contracts: contracts,
		sigs:      sigs,
		tx:        tx,
		publisher: publisher,
	}
}

// WithProofGenerator sets the optional ProofGenerator on the ContractService.
// When non-nil, a proof PDF is generated best-effort after a contract activates.
// Call this after NewContractService to wire the proof pipeline without circular imports.
func (s *ContractService) WithProofGenerator(pg ProofGenerator) {
	s.proofGen = pg
}

// CreateContractInput carries validated input for creating a contract.
// All fields carrying deal-identity (ListingID, AcceptedBidID, FreelancerUserID,
// Amount, Currency) MUST originate from a trusted service-to-service call —
// never from the public API body. The public contract-create endpoint has been
// removed (M-2 fix). Use CreateContractFromAward for the internal S2S path.
type CreateContractInput struct {
	ClientUserID     uuid.UUID // ownerUserId from the marketplace award record
	ListingID        uuid.UUID // authoritative from marketplace award
	AcceptedBidID    uuid.UUID // authoritative from marketplace award (bid_id)
	FreelancerUserID uuid.UUID // authoritative from marketplace award (bidder_user_id)
	Title            string
	Terms            string
	Amount           decimal.Decimal // authoritative from marketplace award
	Currency         string          // authoritative from marketplace award
}

// CreateContractFromAwardInput carries the marketplace-authoritative fields
// needed to create a DRAFT contract from a bid_accepted event. Clients cannot
// supply these values — they are provided exclusively by the marketplace
// service via server-to-server call after AcceptBid succeeds.
type CreateContractFromAwardInput struct {
	// Marketplace-authoritative deal identity — never from client body.
	ListingID        uuid.UUID
	AwardBidID       uuid.UUID // corresponds to accepted_bid_id in contracts table
	ClientUserID     uuid.UUID // listing owner = contract client
	FreelancerUserID uuid.UUID // bid winner = contract freelancer
	Amount           decimal.Decimal
	Currency         string

	// Optional title/terms supplied by marketplace (may be empty; editable later).
	Title string
	Terms string
}

// CreateContractFromAward creates a DRAFT contract from marketplace-authoritative
// award data. This is the ONLY code path for contract creation — the previous
// public POST /v1/contracts endpoint has been removed to close CWE-915/CWE-639.
func (s *ContractService) CreateContractFromAward(ctx context.Context, in *CreateContractFromAwardInput) (*domain.Contract, error) {
	// Title defaults to empty string if marketplace does not supply it; client
	// can fill it in via PATCH before submitting.
	title := in.Title
	if title == "" {
		title = "Contract"
	}

	return s.CreateContract(ctx, &CreateContractInput{
		ClientUserID:     in.ClientUserID,
		ListingID:        in.ListingID,
		AcceptedBidID:    in.AwardBidID,
		FreelancerUserID: in.FreelancerUserID,
		Title:            title,
		Terms:            in.Terms,
		Amount:           in.Amount,
		Currency:         in.Currency,
	})
}

// CreateContract creates a DRAFT contract from an awarded deal.
func (s *ContractService) CreateContract(ctx context.Context, in *CreateContractInput) (*domain.Contract, error) {
	if err := validateTitle(in.Title); err != nil {
		return nil, err
	}

	if err := validateTerms(in.Terms); err != nil {
		return nil, err
	}

	if err := validateCurrency(in.Currency); err != nil {
		return nil, err
	}

	if err := validateAmount(in.Amount); err != nil {
		return nil, err
	}

	if in.ClientUserID == in.FreelancerUserID {
		return nil, fmt.Errorf("%w: client and freelancer must be different users", domain.ErrValidation)
	}

	contractID := uuid.New()
	amountStr := in.Amount.StringFixed(2)
	contentHash := domain.CanonicalContractDigest(
		contractID.String(), in.ClientUserID.String(), in.FreelancerUserID.String(),
		in.Title, in.Terms, amountStr, in.Currency, 1,
	)

	now := time.Now().UTC()
	c := &domain.Contract{
		ID:               contractID,
		ListingID:        in.ListingID,
		AcceptedBidID:    in.AcceptedBidID,
		ClientUserID:     in.ClientUserID,
		FreelancerUserID: in.FreelancerUserID,
		Title:            in.Title,
		Terms:            in.Terms,
		Amount:           in.Amount,
		Currency:         in.Currency,
		ContentHash:      contentHash,
		Version:          1,
		Status:           domain.ContractStatusDraft,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := s.contracts.Create(ctx, c); err != nil {
		return nil, fmt.Errorf("create contract: %w", err)
	}

	return c, nil
}

// GetContract returns a contract by ID. Returns ErrNotFound if caller is not a party.
func (s *ContractService) GetContract(ctx context.Context, id, callerID uuid.UUID) (*domain.Contract, error) {
	c, err := s.contracts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, callerID); err != nil {
		return nil, err
	}

	return c, nil
}

// ListContracts returns contracts where the caller is client or freelancer (self-scoped).
func (s *ContractService) ListContracts(ctx context.Context, filter store.ContractFilter) ([]*domain.Contract, error) {
	return s.contracts.ListByParty(ctx, filter)
}

// PatchContractInput carries validated input for updating a contract.
type PatchContractInput struct {
	ID       uuid.UUID
	CallerID uuid.UUID
	Title    *string
	Terms    *string
	Amount   *decimal.Decimal
	Currency *string
}

// PatchContract applies a partial update to a contract's terms.
// Only allowed in DRAFT or PENDING_SIGNATURE status.
// A terms change bumps version + recomputes content_hash, resets to DRAFT.
// Runs inside a transaction with a FOR UPDATE row lock to prevent concurrent lost-updates.
func (s *ContractService) PatchContract(ctx context.Context, in PatchContractInput) (*domain.Contract, error) {
	var result *domain.Contract

	txErr := s.tx.WithTx(ctx, func(ctx context.Context, txContracts store.ContractStore, _ store.SignatureStore) error {
		c, err := txContracts.GetByIDForUpdate(ctx, in.ID)
		if err != nil {
			return err
		}

		if err := assertParty(c, in.CallerID); err != nil {
			return err
		}

		if !domain.IsEditableContractStatus(c.Status) {
			return fmt.Errorf("%w: contract is not in an editable state", domain.ErrInvalidTransition)
		}

		changed, err := applyPatchFields(c, in)
		if err != nil {
			return err
		}

		if changed {
			c.Version++
			c.Status = domain.ContractStatusDraft // reset to DRAFT, prior sigs invalidated by version bump
			c.ContentHash = domain.CanonicalContractDigest(
				c.ID.String(), c.ClientUserID.String(), c.FreelancerUserID.String(),
				c.Title, c.Terms, c.Amount.StringFixed(2), c.Currency, c.Version,
			)
		}

		if err := txContracts.Update(ctx, c); err != nil {
			return fmt.Errorf("update contract: %w", err)
		}

		result = c

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	return result, nil
}

// applyPatchFields applies the patch fields from in to c, returning true if any field changed.
// Extracted to keep PatchContract within cyclomatic complexity limits.
func applyPatchFields(c *domain.Contract, in PatchContractInput) (bool, error) {
	changed := false

	if in.Title != nil {
		if err := validateTitle(*in.Title); err != nil {
			return false, err
		}

		if *in.Title != c.Title {
			c.Title = *in.Title
			changed = true
		}
	}

	if in.Terms != nil {
		if err := validateTerms(*in.Terms); err != nil {
			return false, err
		}

		if *in.Terms != c.Terms {
			c.Terms = *in.Terms
			changed = true
		}
	}

	if in.Amount != nil {
		if err := validateAmount(*in.Amount); err != nil {
			return false, err
		}

		if !in.Amount.Equal(c.Amount) {
			c.Amount = *in.Amount
			changed = true
		}
	}

	if in.Currency != nil {
		if err := validateCurrency(*in.Currency); err != nil {
			return false, err
		}

		if *in.Currency != c.Currency {
			c.Currency = *in.Currency
			changed = true
		}
	}

	return changed, nil
}

// SubmitContract transitions a DRAFT contract to PENDING_SIGNATURE (client only).
// Runs inside a transaction with a FOR UPDATE row lock to prevent concurrent lost-updates.
func (s *ContractService) SubmitContract(ctx context.Context, id, callerID uuid.UUID) (*domain.Contract, error) {
	var result *domain.Contract

	txErr := s.tx.WithTx(ctx, func(ctx context.Context, txContracts store.ContractStore, _ store.SignatureStore) error {
		c, err := txContracts.GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}

		if err := assertClientOnly(c, callerID); err != nil {
			return err
		}

		if !domain.ValidContractTransition(c.Status, domain.ContractStatusPendingSignature) {
			return fmt.Errorf("%w: contract must be in DRAFT status to submit", domain.ErrInvalidTransition)
		}

		if c.Terms == "" || c.ContentHash == "" {
			return fmt.Errorf("%w: contract must have non-empty terms before submitting", domain.ErrValidation)
		}

		c.Status = domain.ContractStatusPendingSignature

		if err := txContracts.Update(ctx, c); err != nil {
			return fmt.Errorf("submit contract: %w", err)
		}

		result = c

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	return result, nil
}

// SignContractInput carries input for signing a contract.
type SignContractInput struct {
	ContractID        uuid.UUID
	CallerID          uuid.UUID
	SignedContentHash string
	SignerIP          *string
	UserAgent         *string
}

// SignContract records a party's signature. Inside one tx: when both parties have
// signed the same (version, content_hash), the contract transitions PENDING_SIGNATURE
// -> SIGNED -> ACTIVE atomically.
func (s *ContractService) SignContract(ctx context.Context, in SignContractInput) (*domain.Contract, error) {
	var result *domain.Contract

	txErr := s.tx.WithTx(ctx, func(ctx context.Context, txContracts store.ContractStore, txSigs store.SignatureStore) error {
		// FOR UPDATE lock to prevent TOCTOU races (mirrors AcceptBid pattern).
		c, err := txContracts.GetByIDForUpdate(ctx, in.ContractID)
		if err != nil {
			return err
		}

		role, err := deriveSignerRole(c, in.CallerID)
		if err != nil {
			return err
		}

		if c.Status != domain.ContractStatusPendingSignature {
			return fmt.Errorf("%w: contract must be in PENDING_SIGNATURE state to sign", domain.ErrInvalidTransition)
		}

		// Client-submitted hash must match server's authoritative content_hash.
		if in.SignedContentHash != c.ContentHash {
			return domain.ErrHashMismatch
		}

		now := time.Now().UTC()
		sig := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c.ID,
			SignerUserID:      in.CallerID,
			SignerRole:        role,
			ContractVersion:   c.Version,
			SignedContentHash: in.SignedContentHash,
			SignerIP:          in.SignerIP,
			UserAgent:         in.UserAgent,
			SignedAt:          now,
			CreatedAt:         now,
		}

		if createErr := txSigs.Create(ctx, sig); createErr != nil {
			return createErr
		}

		// Evaluate dual-sign completion: count distinct signer_roles for current version+hash.
		count, err := txSigs.CountValidSignatures(ctx, c.ID, c.Version, c.ContentHash)
		if err != nil {
			return fmt.Errorf("count signatures: %w", err)
		}

		// Both parties (CLIENT + FREELANCER) have now signed.
		// State machine: PENDING_SIGNATURE -> SIGNED -> ACTIVE (two-step, both in same tx).
		if count >= 2 {
			// Step 1: transition to SIGNED (validates the PENDING_SIGNATURE -> SIGNED edge).
			if !domain.ValidContractTransition(c.Status, domain.ContractStatusSigned) {
				return fmt.Errorf("%w: cannot transition to SIGNED", domain.ErrInvalidTransition)
			}

			c.Status = domain.ContractStatusSigned

			// Step 2: auto-promote SIGNED -> ACTIVE in the same tx.
			if !domain.ValidContractTransition(c.Status, domain.ContractStatusActive) {
				return fmt.Errorf("%w: cannot transition to ACTIVE", domain.ErrInvalidTransition)
			}

			activatedAt := now
			c.Status = domain.ContractStatusActive
			c.ActivatedAt = &activatedAt

			if updateErr := txContracts.Update(ctx, c); updateErr != nil {
				return fmt.Errorf("activate contract: %w", updateErr)
			}
		}

		result = c

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	// Best-effort: publish event after tx commit. Log failure but don't error.
	if result != nil && result.Status == domain.ContractStatusActive {
		evt := &domain.ContractActivatedEvent{
			EventID:    uuid.New(),
			OccurredAt: time.Now().UTC(),
			Version:    1,
		}
		evt.Data.ContractID = result.ID
		evt.Data.ListingID = result.ListingID
		evt.Data.AcceptedBidID = result.AcceptedBidID
		evt.Data.ClientUserID = result.ClientUserID
		evt.Data.FreelancerUserID = result.FreelancerUserID

		if pubErr := s.publisher.PublishContractActivated(ctx, evt); pubErr != nil {
			slog.Warn("publish contract_activated event failed", "contract_id", result.ID, "err", pubErr)
		}

		s.triggerBilateralProof(result.ID) //nolint:contextcheck // intentional: best-effort goroutine must outlive request context
	}

	return result, nil
}

// triggerBilateralProof launches a best-effort goroutine to generate a proof PDF for
// the given bilateral contract. The goroutine uses context.Background() so it outlives
// the HTTP request context.
func (s *ContractService) triggerBilateralProof(contractID uuid.UUID) {
	// Capture proofGen synchronously (in the caller's goroutine) so the background
	// goroutine never reads the mutable s.proofGen field concurrently with a
	// WithProofGenerator setter (race-free).
	proofGen := s.proofGen
	if proofGen == nil {
		return
	}

	go func() {
		bgCtx := context.Background()
		if _, genErr := proofGen.GenerateAndStore(bgCtx, contractID, domain.ContractKindBilateral); genErr != nil {
			slog.Warn("bilateral proof generation failed (best-effort)",
				"contract_id", contractID, "err", genErr)
		}
	}()
}

// CompleteContract transitions ACTIVE -> COMPLETED (client only).
// Runs inside a transaction with a FOR UPDATE row lock to prevent concurrent lost-updates.
func (s *ContractService) CompleteContract(ctx context.Context, id, callerID uuid.UUID) (*domain.Contract, error) {
	var result *domain.Contract

	txErr := s.tx.WithTx(ctx, func(ctx context.Context, txContracts store.ContractStore, _ store.SignatureStore) error {
		c, err := txContracts.GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}

		if err := assertClientOnly(c, callerID); err != nil {
			return err
		}

		if !domain.ValidContractTransition(c.Status, domain.ContractStatusCompleted) {
			return fmt.Errorf("%w: contract must be in ACTIVE status to complete", domain.ErrInvalidTransition)
		}

		now := time.Now().UTC()
		c.Status = domain.ContractStatusCompleted
		c.CompletedAt = &now

		if err := txContracts.Update(ctx, c); err != nil {
			return fmt.Errorf("complete contract: %w", err)
		}

		result = c

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	return result, nil
}

// CancelContract transitions any non-terminal status to CANCELED (either party).
// Runs inside a transaction with a FOR UPDATE row lock to prevent concurrent lost-updates.
func (s *ContractService) CancelContract(ctx context.Context, id, callerID uuid.UUID) (*domain.Contract, error) {
	var result *domain.Contract

	txErr := s.tx.WithTx(ctx, func(ctx context.Context, txContracts store.ContractStore, _ store.SignatureStore) error {
		c, err := txContracts.GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}

		if err := assertParty(c, callerID); err != nil {
			return err
		}

		if !domain.ValidContractTransition(c.Status, domain.ContractStatusCanceled) {
			return fmt.Errorf("%w: contract cannot be canceled in its current state", domain.ErrInvalidTransition)
		}

		c.Status = domain.ContractStatusCanceled

		if err := txContracts.Update(ctx, c); err != nil {
			return fmt.Errorf("cancel contract: %w", err)
		}

		result = c

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	return result, nil
}
