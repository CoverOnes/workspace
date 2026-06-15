package service

import (
	"context"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// Outbox channel constants for multiparty contract events.
// Must stay in sync with events.channel* constants in the events package.
const (
	channelMultipartyActivated       = "workspace.contract_activated"
	channelMultipartyAddendumCreated = "workspace.contract_addendum_created"
	channelMultipartyReSigned        = "workspace.contract_re_signed"
)

// MultipartyContractService implements the business logic for multi-party N-vendor
// contracts. This is a SEPARATE aggregate from the 1:1 dual-sign ContractService;
// the two do not share state or tables.
//
// Owner-as-party decision: the tender owner is NOT automatically a party with a share.
// Parties are the approved vendor collaborators who hold a role and a share_bps allocation.
// The owner acts as initiator / governance (owner-only governance per Phase 0 locked decisions).
// This is documented here as the canonical decision record.
type MultipartyContractService struct {
	contracts store.MultipartyContractStore
	parties   store.MultipartyPartyStore
	sigs      store.MultipartySignatureStore
	addenda   store.AddendumStore
	tx        store.MultipartyTxManager
	publisher events.Publisher
}

// NewMultipartyContractService returns a MultipartyContractService.
func NewMultipartyContractService(
	contracts store.MultipartyContractStore,
	parties store.MultipartyPartyStore,
	sigs store.MultipartySignatureStore,
	addenda store.AddendumStore,
	tx store.MultipartyTxManager,
	publisher events.Publisher,
) *MultipartyContractService {
	return &MultipartyContractService{
		contracts: contracts,
		parties:   parties,
		sigs:      sigs,
		addenda:   addenda,
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
//   - If the contract is ACTIVE, triggers the addendum flow (Phase 4).
//   - If the contract is in PENDING_SIGNATURES / ADDENDUM_PENDING / COMPLETED / CANCELED,
//     returns ErrInvalidTransition.
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

	switch contract.Status {
	case domain.MultipartyContractStatusDraft:
		return s.addPartyToDraft(ctx, contract, in)

	case domain.MultipartyContractStatusActive:
		return s.addPartyViaAddendum(ctx, contract, in)

	default:
		return nil, nil, fmt.Errorf(
			"%w: cannot add party to contract in status %s",
			domain.ErrInvalidTransition, contract.Status,
		)
	}
}

// addPartyToDraft adds a party directly to a DRAFT contract (original flow).
func (s *MultipartyContractService) addPartyToDraft(
	ctx context.Context,
	contract *domain.MultipartyContract,
	in *CreateOrAddPartyInput,
) (*domain.MultipartyContract, *domain.MultipartyContractParty, error) {
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

// addPartyViaAddendum adds a new party to an ACTIVE contract using the Phase-4
// addendum flow (Model B: add-then-resubmit + re-sign).
//
// Flow:
//  1. Lock contract FOR UPDATE, re-check status == ACTIVE under lock.
//  2. Verify caller is the owner (PosterUserID).
//  3. Insert new party at 0 bps placeholder (23505 → idempotent 409).
//  4. Bump version, recompute digest over ALL ACTIVE parties incl. new one.
//  5. Transition contract: ACTIVE → ADDENDUM_PENDING, update version + digest + party_count.
//  6. Insert contract_addenda row.
//  7. Post-tx publish workspace.contract_addendum_created (best-effort).
func (s *MultipartyContractService) addPartyViaAddendum(
	ctx context.Context,
	contract *domain.MultipartyContract,
	in *CreateOrAddPartyInput,
) (*domain.MultipartyContract, *domain.MultipartyContractParty, error) {
	// Owner-only gate: PosterUserID must be set and match the contract's owner.
	if in.PosterUserID == nil || contract.PosterUserID == nil || *in.PosterUserID != *contract.PosterUserID {
		return nil, nil, fmt.Errorf("%w: only the contract owner may add a party via addendum", domain.ErrForbidden)
	}

	var (
		resultContract *domain.MultipartyContract
		resultParty    *domain.MultipartyContractParty
	)

	txErr := s.tx.WithMultipartyTx(ctx, func(
		txCtx context.Context,
		txContracts store.MultipartyContractStore,
		txParties store.MultipartyPartyStore,
		_ store.MultipartySignatureStore,
		txAddenda store.AddendumStore,
		txOutbox store.OutboxStore,
	) error {
		// Re-fetch under FOR UPDATE lock.
		c, err := txContracts.GetByIDForUpdate(txCtx, contract.ID)
		if err != nil {
			return err
		}

		// Re-check status under lock (concurrent addendum guard).
		if c.Status != domain.MultipartyContractStatusActive {
			return fmt.Errorf("%w: contract is no longer ACTIVE (concurrent addendum?)", domain.ErrInvalidTransition)
		}

		// Defense-in-depth owner check on the LOCKED row (M3 hardening).
		// in.PosterUserID is guaranteed non-nil here (pre-lock guard on line 142).
		// Re-checking against c (the DB-authoritative locked row) closes the TOCTOU window
		// on owner identity: a row-level change between the pre-fetch and the lock cannot
		// sneak through.
		if err := assertLockedOwner(c, *in.PosterUserID); err != nil {
			return err
		}

		now := time.Now().UTC()
		newParty := &domain.MultipartyContractParty{
			ID:           uuid.New(),
			ContractID:   c.ID,
			VendorUserID: in.VendorUserID,
			RoleID:       in.RoleID,
			ShareBps:     0, // placeholder; owner updates shares via PATCH before re-submit
			Status:       domain.MultipartyPartyStatusActive,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		if addErr := txParties.AddParty(txCtx, newParty); addErr != nil {
			return fmt.Errorf("add party via addendum: %w", addErr)
		}

		// Build the new version's roster: all ACTIVE parties incl. the new one.
		activeParties, listErr := txParties.ListActiveByContract(txCtx, c.ID)
		if listErr != nil {
			return fmt.Errorf("list active parties for addendum digest: %w", listErr)
		}

		roster := make([]domain.MultipartyRosterEntry, len(activeParties))
		for i, p := range activeParties {
			roster[i] = domain.MultipartyRosterEntry{
				VendorUserID: p.VendorUserID,
				ShareBps:     p.ShareBps,
			}
		}

		currency := ""
		if c.Currency != nil {
			currency = *c.Currency
		}

		fromVersion := c.Version
		newVersion := c.Version + 1

		c.ContentHash = domain.CanonicalMultipartyDigest(c.TenderID, newVersion, currency, roster)
		c.Status = domain.MultipartyContractStatusAddendumPending
		c.Version = newVersion
		c.PartyCount = len(activeParties)

		if updateErr := txContracts.Update(txCtx, c); updateErr != nil {
			return fmt.Errorf("update contract to ADDENDUM_PENDING: %w", updateErr)
		}

		triggeredBy := *in.PosterUserID
		a := &domain.ContractAddendum{
			ID:              uuid.New(),
			ContractID:      c.ID,
			FromVersion:     fromVersion,
			ToVersion:       newVersion,
			NewPartyID:      newParty.ID,
			NewVendorUserID: in.VendorUserID,
			TriggeredBy:     triggeredBy,
			CreatedAt:       now,
		}

		if addendumErr := txAddenda.Create(txCtx, a); addendumErr != nil {
			return fmt.Errorf("create addendum record: %w", addendumErr)
		}

		resultContract = c
		resultParty = newParty

		return enqueueAddendumCreatedEvent(txCtx, txOutbox, c, a, in.VendorUserID, now)
	})

	if txErr != nil {
		return nil, nil, txErr
	}

	return resultContract, resultParty, nil
}

// enqueueAddendumCreatedEvent enqueues a MultipartyContractAddendumCreatedEvent into the
// outbox. Extracted to keep addPartyViaAddendum below the cyclomatic-complexity ceiling.
func enqueueAddendumCreatedEvent(
	ctx context.Context,
	ob store.OutboxStore,
	c *domain.MultipartyContract,
	a *domain.ContractAddendum,
	newVendorUserID uuid.UUID,
	now time.Time,
) error {
	evt := &domain.MultipartyContractAddendumCreatedEvent{
		EventID:    uuid.New(),
		OccurredAt: now,
		Version:    1,
	}
	evt.Data.ContractID = c.ID
	evt.Data.TenderID = c.TenderID
	evt.Data.FromVersion = a.FromVersion
	evt.Data.ToVersion = a.ToVersion
	evt.Data.NewVendorUserID = newVendorUserID
	evt.Data.PartyCount = c.PartyCount

	payload, marshalErr := marshalEvent(evt)
	if marshalErr != nil {
		return fmt.Errorf("marshal addendum_created event: %w", marshalErr)
	}

	if enqErr := ob.Enqueue(ctx, &store.OutboxEnqueueInput{
		AggregateType: "multiparty_contract",
		AggregateID:   c.ID,
		EventID:       evt.EventID,
		Channel:       channelMultipartyAddendumCreated,
		Payload:       payload,
	}); enqErr != nil {
		return fmt.Errorf("enqueue addendum_created event: %w", enqErr)
	}

	return nil
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

// SubmitForSignatures transitions a DRAFT or ADDENDUM_PENDING contract to PENDING_SIGNATURES.
// Hard gate: Σ(ACTIVE parties' share_bps) MUST equal exactly 10000.
// Runs inside a transaction with FOR UPDATE to prevent concurrent lost-updates.
// Freezes the version's canonical digest (stored as content_hash).
//
// callerUserID MUST be the contract's PosterUserID (owner-only gate).
// Returns ErrForbidden if the caller is not the owner.
func (s *MultipartyContractService) SubmitForSignatures(
	ctx context.Context,
	contractID uuid.UUID,
	callerUserID uuid.UUID,
) (*domain.MultipartyContract, error) {
	var result *domain.MultipartyContract

	txErr := s.tx.WithMultipartyTx(ctx, func(
		ctx context.Context,
		txContracts store.MultipartyContractStore,
		txParties store.MultipartyPartyStore,
		_ store.MultipartySignatureStore,
		_ store.AddendumStore,
		_ store.OutboxStore,
	) error {
		return s.submitForSignaturesTx(ctx, txContracts, txParties, contractID, callerUserID, &result)
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
	callerUserID uuid.UUID,
	result **domain.MultipartyContract,
) error {
	c, err := txContracts.GetByIDForUpdate(ctx, contractID)
	if err != nil {
		return err
	}

	// Owner-only gate: enforce under the row lock to close the TOCTOU window between
	// the HTTP handler's identity extraction and the DB update.
	if err := assertLockedOwner(c, callerUserID); err != nil {
		return err
	}

	if !domain.ValidMultipartyContractTransition(c.Status, domain.MultipartyContractStatusPendingSignatures) {
		return fmt.Errorf("%w: contract must be in DRAFT or ADDENDUM_PENDING status to submit for signatures", domain.ErrInvalidTransition)
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
//   - Validates contract is PENDING_SIGNATURES or ADDENDUM_PENDING.
//   - Validates signed_content_hash == current digest.
//   - Validates version == contract.Version (rejects stale-version signatures).
//   - Inserts the signature row (23505 -> ErrAlreadySigned).
//   - Counts signatures for this version AND counts ACTIVE parties in the same tx.
//   - When signatures == ACTIVE parties -> transitions to ACTIVE, publishes appropriate event.
//
// TOCTOU-safe: SELECT FOR UPDATE on the contract row serializes all concurrent signers.
// Exactly-once activation: the UPDATE to ACTIVE is only executed once (the first tx that
// finds count==parties); subsequent concurrent signers land on a locked row that is already
// ACTIVE and the transition guard rejects them.
func (s *MultipartyContractService) Sign(ctx context.Context, in SignInput) (*domain.MultipartyContract, error) {
	var (
		result        *domain.MultipartyContract
		preSignStatus domain.MultipartyContractStatus
	)

	txErr := s.tx.WithMultipartyTx(ctx, func(
		ctx context.Context,
		txContracts store.MultipartyContractStore,
		txParties store.MultipartyPartyStore,
		txSigs store.MultipartySignatureStore,
		_ store.AddendumStore,
		txOutbox store.OutboxStore,
	) error {
		return s.signTx(ctx, txContracts, txParties, txSigs, txOutbox, in, &result, &preSignStatus)
	})

	if txErr != nil {
		return nil, txErr
	}

	return result, nil
}

func (s *MultipartyContractService) signTx(
	ctx context.Context,
	txContracts store.MultipartyContractStore,
	txParties store.MultipartyPartyStore,
	txSigs store.MultipartySignatureStore,
	txOutbox store.OutboxStore,
	in SignInput,
	result **domain.MultipartyContract,
	preSignStatus *domain.MultipartyContractStatus,
) error {
	// FOR UPDATE lock: serializes all concurrent sign calls on the same contract.
	c, err := txContracts.GetByIDForUpdate(ctx, in.ContractID)
	if err != nil {
		return err
	}

	// Accept signing in both PENDING_SIGNATURES and ADDENDUM_PENDING states.
	if c.Status != domain.MultipartyContractStatusPendingSignatures &&
		c.Status != domain.MultipartyContractStatusAddendumPending {
		return fmt.Errorf("%w: contract must be in PENDING_SIGNATURES or ADDENDUM_PENDING state to sign", domain.ErrInvalidTransition)
	}

	// Invariant guard: a submitted contract must have at least one party. PartyCount==0
	// means SubmitForSignatures was called on an empty roster — the quorum check below
	// (sigCount == c.PartyCount) would never fire, leaving the contract permanently stuck
	// in PENDING_SIGNATURES. Reject early so the caller gets a clear error.
	if c.PartyCount == 0 {
		return fmt.Errorf("%w: contract has no parties; cannot accept signatures", domain.ErrInvalidTransition)
	}

	// Capture pre-sign status so the post-tx publisher can choose the right event.
	*preSignStatus = c.Status

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
		if err := s.activateInTx(ctx, txContracts, txOutbox, c, *preSignStatus); err != nil {
			return err
		}
	}

	*result = c

	return nil
}

func (s *MultipartyContractService) activateInTx(
	ctx context.Context,
	txContracts store.MultipartyContractStore,
	txOutbox store.OutboxStore,
	c *domain.MultipartyContract,
	preSignStatus domain.MultipartyContractStatus,
) error {
	if !domain.ValidMultipartyContractTransition(c.Status, domain.MultipartyContractStatusActive) {
		return fmt.Errorf("%w: cannot transition multiparty contract to ACTIVE", domain.ErrInvalidTransition)
	}

	c.Status = domain.MultipartyContractStatusActive

	if updateErr := txContracts.Update(ctx, c); updateErr != nil {
		return fmt.Errorf("activate multiparty contract: %w", updateErr)
	}

	// Enqueue the correct activation event atomically with the state transition.
	// preSignStatus determines which event: re-sign (after addendum) vs initial activation.
	now := time.Now().UTC()

	if preSignStatus == domain.MultipartyContractStatusAddendumPending {
		evt := &domain.MultipartyContractReSignedEvent{
			EventID:    uuid.New(),
			OccurredAt: now,
			Version:    1,
		}
		evt.Data.ContractID = c.ID
		evt.Data.TenderID = c.TenderID
		evt.Data.NewVersion = c.Version
		evt.Data.PartyCount = c.PartyCount

		payload, marshalErr := marshalEvent(evt)
		if marshalErr != nil {
			return fmt.Errorf("marshal contract_re_signed event: %w", marshalErr)
		}

		if enqErr := txOutbox.Enqueue(ctx, &store.OutboxEnqueueInput{
			AggregateType: "multiparty_contract",
			AggregateID:   c.ID,
			EventID:       evt.EventID,
			Channel:       channelMultipartyReSigned,
			Payload:       payload,
		}); enqErr != nil {
			return fmt.Errorf("enqueue contract_re_signed event: %w", enqErr)
		}
	} else {
		evt := &domain.MultipartyContractActivatedEvent{
			EventID:    uuid.New(),
			OccurredAt: now,
			Version:    1,
		}
		evt.Data.ContractID = c.ID
		evt.Data.TenderID = c.TenderID
		evt.Data.PartyCount = c.PartyCount

		payload, marshalErr := marshalEvent(evt)
		if marshalErr != nil {
			return fmt.Errorf("marshal contract_activated event: %w", marshalErr)
		}

		if enqErr := txOutbox.Enqueue(ctx, &store.OutboxEnqueueInput{
			AggregateType: "multiparty_contract",
			AggregateID:   c.ID,
			EventID:       evt.EventID,
			Channel:       channelMultipartyActivated,
			Payload:       payload,
		}); enqErr != nil {
			return fmt.Errorf("enqueue contract_activated event: %w", enqErr)
		}
	}

	return nil
}

// PartyView is the read model for a party in the contract roster.
// ShareBps is a pointer: it is nil (omitted from JSON) when the caller is not the
// owner and this party is not the caller — i.e. each vendor can only see their own
// share_bps; the tender owner sees everyone's.
type PartyView struct {
	ID           uuid.UUID                    `json:"id"`
	ContractID   uuid.UUID                    `json:"contractId"`
	VendorUserID uuid.UUID                    `json:"vendorUserId"`
	RoleID       *uuid.UUID                   `json:"roleId,omitempty"`
	ShareBps     *int                         `json:"shareBps,omitempty"`
	Status       domain.MultipartyPartyStatus `json:"status"`
	CreatedAt    time.Time                    `json:"createdAt"`
	UpdatedAt    time.Time                    `json:"updatedAt"`
}

// toPartyView converts a domain party row to a PartyView.
// showShare controls whether ShareBps is included or redacted (nil).
func toPartyView(p *domain.MultipartyContractParty, showShare bool) *PartyView {
	pv := &PartyView{
		ID:           p.ID,
		ContractID:   p.ContractID,
		VendorUserID: p.VendorUserID,
		RoleID:       p.RoleID,
		Status:       p.Status,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}

	if showShare {
		bps := p.ShareBps
		pv.ShareBps = &bps
	}

	return pv
}

// ContractDetail carries the full contract read model for the GET endpoint.
// Parties is always populated; ShareBps within each PartyView is only non-nil
// for the party that matches the caller (or for all parties when the caller is the
// tender owner). This enforces per-vendor share_bps confidentiality at the service layer.
type ContractDetail struct {
	Contract       *domain.MultipartyContract
	Parties        []*PartyView
	Signatures     []*domain.MultipartyContractSignature
	SignedCount    int
	TotalParties   int
	ContentHash    string
	CurrentVersion int
}

// GetDetail returns the contract detail visible to the caller.
//
// Access rules:
//   - Tender owner (PosterUserID): reads the full roster with ALL share_bps values.
//   - ACTIVE party: reads the full roster but only their OWN share_bps; every other
//     party's ShareBps is nil (omitted from JSON) to enforce confidentiality.
//   - Non-party / non-owner: ErrNotParty → 404 (prevents resource-existence enumeration).
//
// The redaction is performed at the service layer, not via JSON tags on the domain
// struct, so it applies regardless of which serialiser the handler uses.
func (s *MultipartyContractService) GetDetail(ctx context.Context, contractID, callerUserID uuid.UUID) (*ContractDetail, error) {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	// Owner path: the poster can read the full roster including all share_bps.
	// PosterUserID is nil on legacy rows created before migration 000007 — those rows
	// have no stored owner, so the owner-read path is simply not available for them.
	isOwner := c.PosterUserID != nil && *c.PosterUserID == callerUserID

	if !isOwner {
		// M-3 authz: non-owner must be an ACTIVE party to read any roster data.
		if _, authzErr := s.parties.GetActivePartyByVendor(ctx, contractID, callerUserID); authzErr != nil {
			return nil, authzErr // ErrNotParty → 404
		}
	}

	rawParties, err := s.parties.ListActiveByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list active parties: %w", err)
	}

	sigs, err := s.sigs.ListByContractVersion(ctx, contractID, c.Version)
	if err != nil {
		return nil, fmt.Errorf("list signatures for version: %w", err)
	}

	parties := make([]*PartyView, len(rawParties))
	for i, p := range rawParties {
		// Owner sees all; party sees only their own share.
		showShare := isOwner || p.VendorUserID == callerUserID
		parties[i] = toPartyView(p, showShare)
	}

	return &ContractDetail{
		Contract:       c,
		Parties:        parties,
		Signatures:     sigs,
		SignedCount:    len(sigs),
		TotalParties:   len(rawParties),
		ContentHash:    c.ContentHash,
		CurrentVersion: c.Version,
	}, nil
}

// UpdatePartyShareInput carries input for updating a party's share_bps.
type UpdatePartyShareInput struct {
	ContractID   uuid.UUID
	PartyID      uuid.UUID
	CallerUserID uuid.UUID
	NewShareBps  int
}

// UpdatePartyShare updates a party's share_bps allocation on an ADDENDUM_PENDING contract.
// Rules:
//   - Contract must be ADDENDUM_PENDING.
//   - Caller must be the contract owner (PosterUserID).
//   - newShareBps must be in [0, 10000].
//   - Does NOT recompute content_hash (that happens at SubmitForSignatures).
//
// TOCTOU-safe: the status check and the share update execute inside a single transaction
// with SELECT FOR UPDATE on the contract row. A concurrent SubmitForSignatures cannot
// transition the contract to PENDING_SIGNATURES between the check and the write.
//
// Returns a PartyView with ShareBps set (owner always sees the share they just wrote).
// Using PartyView keeps the serialization format consistent with GetDetail and ensures
// no raw domain.MultipartyContractParty reaches a browser-reachable response body.
func (s *MultipartyContractService) UpdatePartyShare(
	ctx context.Context,
	in UpdatePartyShareInput,
) (*PartyView, error) {
	if err := validateShareBps(in.NewShareBps); err != nil {
		return nil, err
	}

	var updated *domain.MultipartyContractParty

	txErr := s.tx.WithMultipartyTx(ctx, func(
		txCtx context.Context,
		txContracts store.MultipartyContractStore,
		txParties store.MultipartyPartyStore,
		_ store.MultipartySignatureStore,
		_ store.AddendumStore,
		_ store.OutboxStore,
	) error {
		// FOR UPDATE lock: prevents a concurrent SubmitForSignatures from freezing
		// the digest between our status check and the share write.
		c, err := txContracts.GetByIDForUpdate(txCtx, in.ContractID)
		if err != nil {
			return err
		}

		// Re-check status under the lock.
		if c.Status != domain.MultipartyContractStatusAddendumPending {
			return fmt.Errorf("%w: UpdatePartyShare requires ADDENDUM_PENDING status, got %s",
				domain.ErrInvalidTransition, c.Status)
		}

		// Owner-only gate — checked on the locked row.
		if c.PosterUserID == nil || in.CallerUserID != *c.PosterUserID {
			return fmt.Errorf("%w: only the contract owner may update party shares", domain.ErrForbidden)
		}

		result, err := txParties.UpdatePartyShare(txCtx, in.ContractID, in.PartyID, in.NewShareBps)
		if err != nil {
			return err
		}

		updated = result

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	// Owner always sees the share they just wrote (showShare=true).
	return toPartyView(updated, true), nil
}

// assertLockedOwner checks that the DB-authoritative locked contract row has a PosterUserID
// that matches callerID. Used inside transactions after GetByIDForUpdate to close the TOCTOU
// window between the pre-lock owner check and the locked row read (M3 hardening).
func assertLockedOwner(c *domain.MultipartyContract, callerID uuid.UUID) error {
	if c.PosterUserID == nil || *c.PosterUserID != callerID {
		return fmt.Errorf("%w: only the contract owner may perform this operation (locked row check)", domain.ErrForbidden)
	}

	return nil
}

// validateShareBps validates that share_bps is in [0, 10000].
func validateShareBps(bps int) error {
	if bps < 0 || bps > 10000 {
		return fmt.Errorf("%w: share_bps must be between 0 and 10000, got %d", domain.ErrValidation, bps)
	}

	return nil
}
