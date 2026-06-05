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
)

// MultipartyContractService implements the business logic for multi-party N-vendor
// contracts. This is a SEPARATE aggregate from the 1:1 dual-sign ContractService;
// the two do not share state or tables.
//
// Owner-as-party decision: the tender owner is NOT automatically a party with a share.
// Parties are the approved vendor collaborators who hold a role and a share_bps allocation.
// The owner acts as initiator / governance (owner-only governance per Phase 0 locked decisions).
// This is documented here as the canonical decision record.
//
// TODO Phase 4: when adding a party to an ACTIVE contract, bump version and initiate
// addendum re-sign so all existing parties must re-sign the new roster.
type MultipartyContractService struct {
	contracts store.MultipartyContractStore
	parties   store.MultipartyPartyStore
	sigs      store.MultipartySignatureStore
	tx        store.MultipartyTxManager
	publisher events.Publisher
}

// NewMultipartyContractService returns a MultipartyContractService.
func NewMultipartyContractService(
	contracts store.MultipartyContractStore,
	parties store.MultipartyPartyStore,
	sigs store.MultipartySignatureStore,
	tx store.MultipartyTxManager,
	publisher events.Publisher,
) *MultipartyContractService {
	return &MultipartyContractService{
		contracts: contracts,
		parties:   parties,
		sigs:      sigs,
		tx:        tx,
		publisher: publisher,
	}
}

// CreateOrAddPartyInput carries S2S-validated input for idempotent contract creation
// and party addition. Called by marketplace when an approved collaborator is added.
// All fields are marketplace-authoritative; this endpoint is S2S only, not browser-facing.
type CreateOrAddPartyInput struct {
	TenderID     uuid.UUID
	VendorUserID uuid.UUID
	RoleID       *uuid.UUID
	ShareBps     int
	Currency     *string    // optional; only considered at creation time
	PosterUserID *uuid.UUID // optional; the tender owner; stored on first contract creation only
}

// CreateOrAddParty is the idempotent S2S endpoint:
//   - If no live contract exists for TenderID, create one and add the party.
//   - If a live DRAFT contract already exists, add the party to it (idempotent on
//     vendor_user_id: returns ErrConflict if the vendor already has an ACTIVE row).
//   - If the contract is not in DRAFT status, returns ErrInvalidTransition — adding
//     parties to non-DRAFT contracts requires the addendum flow (TODO Phase 4).
//
// Returns the contract and the new party row.
func (s *MultipartyContractService) CreateOrAddParty(
	ctx context.Context,
	in *CreateOrAddPartyInput,
) (*domain.MultipartyContract, *domain.MultipartyContractParty, error) {
	if err := validateShareBps(in.ShareBps); err != nil {
		return nil, nil, err
	}

	contract, err := s.getOrCreateContract(ctx, in)
	if err != nil {
		return nil, nil, err
	}

	// Gate: parties can only be added to DRAFT contracts.
	// Adding to ACTIVE/PENDING_SIGNATURES triggers addendum (TODO Phase 4).
	if contract.Status != domain.MultipartyContractStatusDraft {
		return nil, nil, fmt.Errorf(
			"%w: parties can only be added to DRAFT contracts (addendum flow TODO Phase 4)",
			domain.ErrInvalidTransition,
		)
	}

	now := time.Now().UTC()
	party := &domain.MultipartyContractParty{
		ID:           uuid.New(),
		ContractID:   contract.ID,
		VendorUserID: in.VendorUserID,
		RoleID:       in.RoleID,
		ShareBps:     in.ShareBps,
		Status:       domain.MultipartyPartyStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.parties.AddParty(ctx, party); err != nil {
		return nil, nil, fmt.Errorf("add party: %w", err)
	}

	return contract, party, nil
}

// getOrCreateContract fetches the live contract for a tender, or creates a new DRAFT.
// On concurrent creation, the loser retries GetByTenderID to fetch the winner's row.
func (s *MultipartyContractService) getOrCreateContract(
	ctx context.Context,
	in *CreateOrAddPartyInput,
) (*domain.MultipartyContract, error) {
	existing, err := s.contracts.GetByTenderID(ctx, in.TenderID)
	if err != nil && err != domain.ErrMultipartyContractNotFound {
		return nil, fmt.Errorf("get multiparty contract by tender: %w", err)
	}

	if existing != nil {
		return existing, nil
	}

	now := time.Now().UTC()
	contract := &domain.MultipartyContract{
		ID:           uuid.New(),
		TenderID:     in.TenderID,
		Status:       domain.MultipartyContractStatusDraft,
		ContentHash:  "", // computed at submit-for-signature
		Version:      1,
		Currency:     in.Currency,
		PosterUserID: in.PosterUserID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if createErr := s.contracts.Create(ctx, contract); createErr != nil {
		// Concurrent creation: another goroutine won the race; fetch the winner.
		if createErr == domain.ErrConflict {
			winner, fetchErr := s.contracts.GetByTenderID(ctx, in.TenderID)
			if fetchErr != nil {
				return nil, fmt.Errorf("fetch winner after concurrent create: %w", fetchErr)
			}

			return winner, nil
		}

		return nil, fmt.Errorf("create multiparty contract: %w", createErr)
	}

	return contract, nil
}

// SubmitForSignatures transitions a DRAFT contract to PENDING_SIGNATURES.
// Hard gate: Σ(ACTIVE parties' share_bps) MUST equal exactly 10000.
// Runs inside a transaction with FOR UPDATE to prevent concurrent lost-updates.
// Freezes the version's canonical digest (stored as content_hash).
func (s *MultipartyContractService) SubmitForSignatures(
	ctx context.Context,
	contractID uuid.UUID,
) (*domain.MultipartyContract, error) {
	var result *domain.MultipartyContract

	txErr := s.tx.WithMultipartyTx(ctx, func(
		ctx context.Context,
		txContracts store.MultipartyContractStore,
		txParties store.MultipartyPartyStore,
		_ store.MultipartySignatureStore,
	) error {
		return s.submitForSignaturesTx(ctx, txContracts, txParties, contractID, &result)
	})

	if txErr != nil {
		return nil, txErr
	}

	return result, nil
}

func (s *MultipartyContractService) submitForSignaturesTx(
	ctx context.Context,
	txContracts store.MultipartyContractStore,
	txParties store.MultipartyPartyStore,
	contractID uuid.UUID,
	result **domain.MultipartyContract,
) error {
	c, err := txContracts.GetByIDForUpdate(ctx, contractID)
	if err != nil {
		return err
	}

	if !domain.ValidMultipartyContractTransition(c.Status, domain.MultipartyContractStatusPendingSignatures) {
		return fmt.Errorf("%w: contract must be in DRAFT status to submit for signatures", domain.ErrInvalidTransition)
	}

	// Hard gate: Σ of ACTIVE party share_bps must equal exactly 10000.
	sum, err := txParties.SumActiveBps(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("sum active share_bps: %w", err)
	}

	if sum != 10000 {
		return fmt.Errorf("%w: got %d, need 10000", domain.ErrShareSumNotFull, sum)
	}

	// Build roster and compute canonical digest.
	activeParties, err := txParties.ListActiveByContract(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("list active parties for digest: %w", err)
	}

	roster := make([]domain.MultipartyRosterEntry, len(activeParties))
	for i, p := range activeParties {
		roster[i] = domain.MultipartyRosterEntry{
			VendorUserID: p.VendorUserID,
			ShareBps:     p.ShareBps,
		}
	}

	// Freeze currency for digest: use empty string if not set (still immutable once parties exist).
	currency := ""
	if c.Currency != nil {
		currency = *c.Currency
	}

	c.ContentHash = domain.CanonicalMultipartyDigest(c.TenderID, c.Version, currency, roster)
	c.Status = domain.MultipartyContractStatusPendingSignatures

	// Freeze the party count at submit time (M-2 fix: quorum uses this frozen value,
	// not a live COUNT(*), so roster shrink after submit cannot lower the threshold).
	c.PartyCount = len(activeParties)

	if updateErr := txContracts.Update(ctx, c); updateErr != nil {
		return fmt.Errorf("update contract to PENDING_SIGNATURES: %w", updateErr)
	}

	*result = c

	return nil
}

// SignInput carries input for a party signing a multi-party contract.
type SignInput struct {
	ContractID        uuid.UUID
	SignerUserID      uuid.UUID
	SignedContentHash string
	Version           int
}

// Sign records a party's signature. Inside one tx (SELECT FOR UPDATE):
//   - Validates contract is PENDING_SIGNATURES.
//   - Validates signed_content_hash == current digest.
//   - Validates version == contract.Version (rejects stale-version signatures).
//   - Inserts the signature row (23505 -> ErrAlreadySigned).
//   - Counts signatures for this version AND counts ACTIVE parties in the same tx.
//   - When signatures == ACTIVE parties -> transitions to ACTIVE, publishes event.
//
// TOCTOU-safe: SELECT FOR UPDATE on the contract row serializes all concurrent signers.
// Exactly-once activation: the UPDATE to ACTIVE is only executed once (the first tx that
// finds count==parties); subsequent concurrent signers land on a locked row that is already
// ACTIVE and the transition guard rejects them.
func (s *MultipartyContractService) Sign(ctx context.Context, in SignInput) (*domain.MultipartyContract, error) {
	var result *domain.MultipartyContract

	txErr := s.tx.WithMultipartyTx(ctx, func(
		ctx context.Context,
		txContracts store.MultipartyContractStore,
		txParties store.MultipartyPartyStore,
		txSigs store.MultipartySignatureStore,
	) error {
		return s.signTx(ctx, txContracts, txParties, txSigs, in, &result)
	})

	if txErr != nil {
		return nil, txErr
	}

	// Best-effort event publish after tx commit.
	if result != nil && result.Status == domain.MultipartyContractStatusActive {
		s.publishActivated(ctx, result)
	}

	return result, nil
}

func (s *MultipartyContractService) signTx(
	ctx context.Context,
	txContracts store.MultipartyContractStore,
	txParties store.MultipartyPartyStore,
	txSigs store.MultipartySignatureStore,
	in SignInput,
	result **domain.MultipartyContract,
) error {
	// FOR UPDATE lock: serializes all concurrent sign calls on the same contract.
	c, err := txContracts.GetByIDForUpdate(ctx, in.ContractID)
	if err != nil {
		return err
	}

	if c.Status != domain.MultipartyContractStatusPendingSignatures {
		return fmt.Errorf("%w: contract must be in PENDING_SIGNATURES state to sign", domain.ErrInvalidTransition)
	}

	// Reject stale-version signatures (version must match current contract version).
	if in.Version != c.Version {
		return fmt.Errorf("%w: signer submitted version %d, current version is %d",
			domain.ErrStaleVersion, in.Version, c.Version)
	}

	// Reject if signed hash does not match the server's authoritative digest.
	if in.SignedContentHash != c.ContentHash {
		return domain.ErrHashMismatch
	}

	// C-1 authz: verify the signer is an ACTIVE party BEFORE creating the signature row.
	// The UNIQUE index prevents duplicate signatures but does NOT enforce party membership —
	// any authenticated user could otherwise sign any contract they know the hash of.
	if _, err := txParties.GetActivePartyByVendor(ctx, c.ID, in.SignerUserID); err != nil {
		return err // ErrNotParty → 404 via httpx.Err
	}

	now := time.Now().UTC()
	sig := &domain.MultipartyContractSignature{
		ID:                uuid.New(),
		ContractID:        c.ID,
		SignerUserID:      in.SignerUserID,
		Version:           c.Version,
		SignedContentHash: in.SignedContentHash,
		SignedAt:          now,
		CreatedAt:         now,
	}

	if createErr := txSigs.Create(ctx, sig); createErr != nil {
		return createErr
	}

	// Quorum check: count signatures for this version and compare against the
	// FROZEN party_count (set at SubmitForSignatures time). Using the frozen count
	// closes the Phase-4 footgun where a roster shrink after submit could lower the
	// live COUNT(*) and trigger premature activation (M-2 fix).
	sigCount, err := txSigs.CountSignaturesForVersion(ctx, c.ID, c.Version)
	if err != nil {
		return fmt.Errorf("count signatures: %w", err)
	}

	if c.PartyCount > 0 && sigCount == c.PartyCount {
		if err := s.activateInTx(ctx, txContracts, c); err != nil {
			return err
		}
	}

	*result = c

	return nil
}

func (s *MultipartyContractService) activateInTx(
	ctx context.Context,
	txContracts store.MultipartyContractStore,
	c *domain.MultipartyContract,
) error {
	if !domain.ValidMultipartyContractTransition(c.Status, domain.MultipartyContractStatusActive) {
		return fmt.Errorf("%w: cannot transition multiparty contract to ACTIVE", domain.ErrInvalidTransition)
	}

	c.Status = domain.MultipartyContractStatusActive

	if updateErr := txContracts.Update(ctx, c); updateErr != nil {
		return fmt.Errorf("activate multiparty contract: %w", updateErr)
	}

	return nil
}

func (s *MultipartyContractService) publishActivated(ctx context.Context, contract *domain.MultipartyContract) {
	activeParties, listErr := s.parties.ListActiveByContract(ctx, contract.ID)
	partyCount := 0

	if listErr != nil {
		slog.Warn("list active parties for event publish failed",
			"contract_id", contract.ID, "err", listErr)
	} else {
		partyCount = len(activeParties)
	}

	evt := &domain.MultipartyContractActivatedEvent{
		EventID:    uuid.New(),
		OccurredAt: time.Now().UTC(),
		Version:    1,
	}
	evt.Data.ContractID = contract.ID
	evt.Data.TenderID = contract.TenderID
	evt.Data.PartyCount = partyCount

	if pubErr := s.publisher.PublishMultipartyContractActivated(ctx, evt); pubErr != nil {
		slog.Warn("publish multiparty contract_activated event failed",
			"contract_id", contract.ID, "err", pubErr)
	}
}

// ContractDetail carries the full contract read model for the GET endpoint.
type ContractDetail struct {
	Contract       *domain.MultipartyContract
	Parties        []*domain.MultipartyContractParty
	Signatures     []*domain.MultipartyContractSignature
	SignedCount    int
	TotalParties   int
	ContentHash    string
	CurrentVersion int
}

// GetDetail returns the full contract detail: contract + roster + per-version
// signature progress (signed_count / total_active_parties / version_content_hash).
//
// Access is scoped to ACTIVE parties of the contract. A non-party caller receives
// ErrNotParty (mapped to 404 to prevent resource-existence enumeration), mirroring
// the assertParty pattern used by the 1:1 dual-sign aggregate.
//
// NOTE: the tender OWNER is not a party per the owner-as-party locked decision
// (see service-level comment and SubmitForSignatures). Owner-read access would need
// a separate owner-only endpoint or a broader identity check not in scope for this PR.
// TODO: if owner-read is required, add GetDetailByOwner that accepts the tenderID and
// validates ownership against marketplace claims, then grant a read-only view without
// share_bps details.
func (s *MultipartyContractService) GetDetail(ctx context.Context, contractID, callerUserID uuid.UUID) (*ContractDetail, error) {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	// M-3 authz: verify the caller is an ACTIVE party before returning the full roster
	// (which includes share_bps and content_hash). A non-party user who knows the
	// contract ID can otherwise read the full digest needed to exploit C-1.
	if _, authzErr := s.parties.GetActivePartyByVendor(ctx, contractID, callerUserID); authzErr != nil {
		return nil, authzErr // ErrNotParty → 404
	}

	parties, err := s.parties.ListActiveByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list active parties: %w", err)
	}

	sigs, err := s.sigs.ListByContractVersion(ctx, contractID, c.Version)
	if err != nil {
		return nil, fmt.Errorf("list signatures for version: %w", err)
	}

	return &ContractDetail{
		Contract:       c,
		Parties:        parties,
		Signatures:     sigs,
		SignedCount:    len(sigs),
		TotalParties:   len(parties),
		ContentHash:    c.ContentHash,
		CurrentVersion: c.Version,
	}, nil
}

// validateShareBps validates that share_bps is in [0, 10000].
func validateShareBps(bps int) error {
	if bps < 0 || bps > 10000 {
		return fmt.Errorf("%w: share_bps must be between 0 and 10000, got %d", domain.ErrValidation, bps)
	}

	return nil
}
