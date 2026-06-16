package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/fileclient"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// proofFileClient is the narrow interface ProofService uses to interact with the
// file service. Using fileclient types directly (no mirroring) so *fileclient.Client
// satisfies this interface without an adapter.
// proofFileClient is unexported; callers assign concrete types via ProofServiceConfig.FileClient.
type proofFileClient interface {
	// StoreSystemFile stores a system-generated file and returns its assigned IDs.
	StoreSystemFile(ctx context.Context, in fileclient.StoreSystemFileInput) (*fileclient.StoreSystemFileResult, error)
	// PresignDownload returns a short-lived / TTL-limited presigned download URL for a stored file.
	PresignDownload(ctx context.Context, fileID uuid.UUID) (url string, ttlSeconds int, err error)
}

// ProofServiceConfig holds all dependencies for constructing a ProofService.
type ProofServiceConfig struct {
	// ProofStore persists proof metadata rows.
	ProofStore store.ContractProofStore
	// AuditStore is used to fetch the audit chain tail hash at proof generation time.
	AuditStore store.ContractAuditLogStore
	// ContractStore reads bilateral contracts.
	ContractStore store.ContractStore
	// SignatureStore reads bilateral signatures.
	SignatureStore store.SignatureStore
	// MultipartyContractStore reads multiparty contracts.
	MultipartyContractStore store.MultipartyContractStore
	// MultipartyPartyStore reads active multiparty parties (used for authz + PDF content).
	MultipartyPartyStore store.MultipartyPartyStore
	// MultipartySignatureStore reads multiparty signatures for the proof document.
	MultipartySignatureStore store.MultipartySignatureStore
	// FileClient stores the generated PDF and produces short-lived / TTL-limited
	// presigned download URLs. Must implement proofFileClient.
	FileClient proofFileClient
}

// ProofService orchestrates proof generation, storage, and download URL issuance
// for both bilateral and multiparty contracts.
type ProofService struct {
	proofStore  store.ContractProofStore
	auditStore  store.ContractAuditLogStore
	contracts   store.ContractStore
	sigs        store.SignatureStore
	mpContracts store.MultipartyContractStore
	mpParties   store.MultipartyPartyStore
	mpSigs      store.MultipartySignatureStore
	fileClient  proofFileClient
}

// NewProofService returns a ProofService. Returns an error if any required dependency
// in cfg is nil.
func NewProofService(cfg *ProofServiceConfig) (*ProofService, error) {
	if cfg.ProofStore == nil {
		return nil, fmt.Errorf("ProofService: ProofStore is required")
	}

	if cfg.AuditStore == nil {
		return nil, fmt.Errorf("ProofService: AuditStore is required")
	}

	if cfg.ContractStore == nil {
		return nil, fmt.Errorf("ProofService: ContractStore is required")
	}

	if cfg.SignatureStore == nil {
		return nil, fmt.Errorf("ProofService: SignatureStore is required")
	}

	if cfg.MultipartyContractStore == nil {
		return nil, fmt.Errorf("ProofService: MultipartyContractStore is required")
	}

	if cfg.MultipartyPartyStore == nil {
		return nil, fmt.Errorf("ProofService: MultipartyPartyStore is required")
	}

	if cfg.MultipartySignatureStore == nil {
		return nil, fmt.Errorf("ProofService: MultipartySignatureStore is required")
	}

	if cfg.FileClient == nil {
		return nil, fmt.Errorf("ProofService: FileClient is required")
	}

	return &ProofService{
		proofStore:  cfg.ProofStore,
		auditStore:  cfg.AuditStore,
		contracts:   cfg.ContractStore,
		sigs:        cfg.SignatureStore,
		mpContracts: cfg.MultipartyContractStore,
		mpParties:   cfg.MultipartyPartyStore,
		mpSigs:      cfg.MultipartySignatureStore,
		fileClient:  cfg.FileClient,
	}, nil
}

// GenerateAndStore generates a tamper-evidence signed-contract proof PDF for the
// given contract, stores it via the file service, and records a contract_proofs row.
//
// Version-aware idempotency:
//   - Same contract_version as the existing row → return existing proof (idempotent skip).
//   - Older contract_version in the existing row → SUPERSEDE: regenerate PDF + re-store +
//     UPDATE the row in place (new file_id, object_key, sha256, audit_chain_head,
//     contract_version, generated_at). Used after addendum re-sign.
//   - No existing row → insert new row.
//
// The fetched contract MUST be in ACTIVE status; returns an error otherwise.
//
// Parameters:
//   - ctx: caller context. For goroutine-based best-effort calls, pass context.Background().
//   - contractID: the UUID of the signed contract.
//   - kind: domain.ContractKindBilateral or domain.ContractKindMultiparty.
//
// Returns the stored proof record, or an error.
func (s *ProofService) GenerateAndStore(
	ctx context.Context,
	contractID uuid.UUID,
	kind domain.ContractKind,
) (*domain.ContractProof, error) {
	// Assemble the proof document from contract data (also validates ACTIVE status).
	doc, err := s.buildProofDocument(ctx, contractID, kind)
	if err != nil {
		return nil, fmt.Errorf("build proof document: %w", err)
	}

	// Version-aware idempotency check: fetch any existing proof row.
	existing, err := s.proofStore.GetByContract(ctx, contractID, kind)
	if err != nil && err != store.ErrProofNotFound {
		return nil, fmt.Errorf("check existing proof: %w", err)
	}

	if existing != nil {
		if existing.ContractVersion == doc.Version {
			// Same version: idempotent skip — return the existing proof unchanged.
			return existing, nil
		}

		// Older version: supersede — regenerate and UPDATE the row in place.
		return s.supersede(ctx, existing, doc)
	}

	// No existing row: generate and insert.
	return s.generateNew(ctx, doc)
}

// generateNew renders the PDF, uploads it, and inserts a new proof row.
func (s *ProofService) generateNew(ctx context.Context, doc *domain.ProofDocument) (*domain.ContractProof, error) {
	sha256Hex, storeResult, err := s.renderUpload(ctx, doc)
	if err != nil {
		return nil, err
	}

	proof := &domain.ContractProof{
		ID:              uuid.New(),
		ContractID:      doc.ContractID,
		ContractKind:    doc.ContractKind,
		ContractVersion: doc.Version,
		FileID:          storeResult.FileID,
		ObjectKey:       storeResult.ObjectKey,
		SHA256:          sha256Hex,
		AuditChainHead:  doc.AuditChainHead,
		GeneratedAt:     doc.GeneratedAt,
	}

	if createErr := s.proofStore.Create(ctx, proof); createErr != nil {
		// Concurrent race: another goroutine generated and stored simultaneously.
		if createErr == store.ErrProofAlreadyExists {
			winner, fetchErr := s.proofStore.GetByContract(ctx, doc.ContractID, doc.ContractKind)
			if fetchErr != nil {
				return nil, fmt.Errorf("fetch proof after concurrent create: %w", fetchErr)
			}

			return winner, nil
		}

		return nil, fmt.Errorf("persist proof record: %w", createErr)
	}

	return proof, nil
}

// supersede re-renders the PDF for a new contract version and UPDATE the existing
// proof row in place. The proof ID and contract_id are preserved; file_id, object_key,
// sha256, audit_chain_head, contract_version, and generated_at are replaced.
func (s *ProofService) supersede(
	ctx context.Context,
	existing *domain.ContractProof,
	doc *domain.ProofDocument,
) (*domain.ContractProof, error) {
	sha256Hex, storeResult, err := s.renderUpload(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("supersede: %w", err)
	}

	updated := &domain.ContractProof{
		ID:              existing.ID,
		ContractID:      existing.ContractID,
		ContractKind:    existing.ContractKind,
		ContractVersion: doc.Version,
		FileID:          storeResult.FileID,
		ObjectKey:       storeResult.ObjectKey,
		SHA256:          sha256Hex,
		AuditChainHead:  doc.AuditChainHead,
		GeneratedAt:     doc.GeneratedAt,
	}

	if supErr := s.proofStore.Supersede(ctx, updated); supErr != nil {
		return nil, fmt.Errorf("supersede proof row: %w", supErr)
	}

	slog.Debug("proof superseded by new contract version",
		"contract_id", existing.ContractID,
		"kind", existing.ContractKind,
		"old_version", existing.ContractVersion,
		"new_version", doc.Version)

	return updated, nil
}

// renderUpload renders the proof PDF, computes SHA-256, and uploads via the file service.
// Returns the sha256 hex string and the store result on success.
// SystemContext is set to a deterministic path derived from (contractID, kind, version)
// so that a retry after a DB failure overwrites the orphaned file rather than creating
// a new one. File-service GC of truly orphaned objects is a separate file-service task.
func (s *ProofService) renderUpload(
	ctx context.Context,
	doc *domain.ProofDocument,
) (string, *fileclient.StoreSystemFileResult, error) {
	pdfBytes, err := RenderProofPDF(doc)
	if err != nil {
		return "", nil, fmt.Errorf("render proof PDF: %w", err)
	}

	digest := sha256.Sum256(pdfBytes)
	sha256Hex := hex.EncodeToString(digest[:])

	// Deterministic idempotency key: a retry after DB failure overwrites the orphaned
	// file in the file service rather than creating a new one. File-service GC of
	// truly-orphaned objects is a separate file-service maintenance task.
	objectKeyHint := fmt.Sprintf("contract-proof/%s/%s/v%d.pdf",
		doc.ContractID, doc.ContractKind, doc.Version)

	storeResult, err := s.fileClient.StoreSystemFile(ctx, fileclient.StoreSystemFileInput{
		ContentType:   "application/pdf",
		Filename:      fmt.Sprintf("contract-proof-%s.pdf", doc.ContractID),
		Data:          pdfBytes,
		SystemContext: objectKeyHint,
	})
	if err != nil {
		return "", nil, fmt.Errorf("store proof PDF: %w", err)
	}

	return sha256Hex, storeResult, nil
}

// GetDownloadURL returns a short-lived / TTL-limited presigned download URL for a contract proof.
// Authz: the caller MUST be a party to the contract; non-parties receive domain.ErrForbidden.
//
// Parameters:
//   - ctx: request context.
//   - contractID: the contract UUID.
//   - kind: bilateral or multiparty.
//   - callerID: the authenticated user requesting the download URL.
//
// Returns the presigned URL string, its TTL in seconds, or an error.
func (s *ProofService) GetDownloadURL(
	ctx context.Context,
	contractID uuid.UUID,
	kind domain.ContractKind,
	callerID uuid.UUID,
) (downloadURL string, ttlSeconds int, err error) {
	// Server-side authz: never trust caller-supplied party claim.
	if authzErr := s.assertProofParty(ctx, contractID, kind, callerID); authzErr != nil {
		return "", 0, authzErr
	}

	proof, err := s.proofStore.GetByContract(ctx, contractID, kind)
	if err != nil {
		if err == store.ErrProofNotFound {
			return "", 0, domain.ErrNotFound
		}

		return "", 0, fmt.Errorf("get proof for download: %w", err)
	}

	downloadURL, ttlSeconds, err = s.fileClient.PresignDownload(ctx, proof.FileID)
	if err != nil {
		return "", 0, fmt.Errorf("presign download for proof %s: %w", proof.ID, err)
	}

	// Validate URL scheme — reject non-HTTPS to catch misconfigured file service.
	if !strings.HasPrefix(downloadURL, "https://") {
		return "", 0, fmt.Errorf("file service returned non-https presigned URL (file_id=%s)", proof.FileID)
	}

	// Log file_id only — never the full URL (contains signed credentials).
	slog.Debug("proof download URL issued",
		"contract_id", contractID, "kind", kind, "file_id", proof.FileID)

	return downloadURL, ttlSeconds, nil
}

// assertProofParty verifies that callerID is a party to the given contract.
// Returns domain.ErrForbidden for non-parties.
// The proof endpoint uses ErrForbidden (not ErrNotFound) because the contract ID
// is already known to the caller (they navigated to /contracts/:id/proof).
//
// Bilateral: allows the two contract parties (client + freelancer).
//
// Multiparty: allows any active vendor-party (GetActivePartyByVendor) AND the
// contract poster (multiparty_contracts.poster_user_id). The poster is the user
// who created the contract; they do not appear in multiparty_parties so they
// would receive ErrForbidden without this explicit check.
//
// Note on bilateral vs multiparty divergence: bilateral proof returns ErrForbidden
// (not ErrNotFound) for non-parties — this is intentional (both kinds use 403 so
// callers cannot probe contract existence via the proof endpoint).
func (s *ProofService) assertProofParty(
	ctx context.Context,
	contractID uuid.UUID,
	kind domain.ContractKind,
	callerID uuid.UUID,
) error {
	switch kind {
	case domain.ContractKindBilateral:
		c, err := s.contracts.GetByID(ctx, contractID)
		if err != nil {
			return err
		}

		if callerID != c.ClientUserID && callerID != c.FreelancerUserID {
			return domain.ErrForbidden
		}

		return nil

	case domain.ContractKindMultiparty:
		// Allow the contract poster explicitly — they do not appear in multiparty_parties
		// (which only stores vendor rows) so GetActivePartyByVendor would return ErrNotParty.
		mc, err := s.mpContracts.GetByID(ctx, contractID)
		if err != nil {
			return err
		}

		if mc.PosterUserID != nil && callerID == *mc.PosterUserID {
			return nil // poster is authorized
		}

		_, partyErr := s.mpParties.GetActivePartyByVendor(ctx, contractID, callerID)
		if partyErr != nil {
			// Only ErrNotParty from the party store maps to 403.
			// All other errors (DB failure, context cancel) propagate unchanged.
			if errors.Is(partyErr, domain.ErrNotParty) {
				return domain.ErrForbidden
			}

			return partyErr
		}

		return nil

	default:
		return fmt.Errorf("%w: unknown contract kind %q", domain.ErrValidation, kind)
	}
}

// buildProofDocument assembles a ProofDocument from the stored contract data.
// Returns an error if the contract is not ACTIVE — proof generation requires a fully
// signed contract.
func (s *ProofService) buildProofDocument(
	ctx context.Context,
	contractID uuid.UUID,
	kind domain.ContractKind,
) (*domain.ProofDocument, error) {
	// Fetch audit chain head. An empty string is valid (no audit entries yet).
	chainHead, err := s.auditStore.TailHash(ctx, contractID)
	if err != nil {
		slog.Warn("proof: failed to fetch audit chain head; using empty string",
			"contract_id", contractID, "err", err)

		chainHead = ""
	}

	generatedAt := time.Now().UTC()

	switch kind {
	case domain.ContractKindBilateral:
		return s.buildBilateralDocument(ctx, contractID, chainHead, generatedAt)
	case domain.ContractKindMultiparty:
		return s.buildMultipartyDocument(ctx, contractID, chainHead, generatedAt)
	default:
		return nil, fmt.Errorf("%w: unknown contract kind %q", domain.ErrValidation, kind)
	}
}

// buildBilateralDocument assembles ProofDocument for a 1:1 bilateral contract.
// Returns ErrInvalidTransition if the contract is not ACTIVE.
func (s *ProofService) buildBilateralDocument(
	ctx context.Context,
	contractID uuid.UUID,
	chainHead string,
	generatedAt time.Time,
) (*domain.ProofDocument, error) {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("get bilateral contract %s: %w", contractID, err)
	}

	if c.Status != domain.ContractStatusActive {
		return nil, fmt.Errorf("%w: bilateral contract %s is not ACTIVE (status=%s)",
			domain.ErrInvalidTransition, contractID, c.Status)
	}

	sigs, err := s.sigs.ListByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list bilateral signatures for %s: %w", contractID, err)
	}

	// Include only signatures for the current (final) version — earlier versions
	// correspond to pre-activation edits and are not part of the proof.
	entries := make([]domain.ProofSignerEntry, 0, len(sigs))
	for _, sig := range sigs {
		if sig.ContractVersion != c.Version {
			continue
		}

		entries = append(entries, domain.ProofSignerEntry{
			UserID:   sig.SignerUserID,
			Role:     string(sig.SignerRole),
			SignedAt: sig.SignedAt.UTC(),
		})
	}

	return &domain.ProofDocument{
		ContractID:     contractID,
		ContractKind:   domain.ContractKindBilateral,
		Title:          c.Title,
		TermsSummary:   c.Terms,
		Version:        c.Version,
		Signers:        entries,
		AuditChainHead: chainHead,
		GeneratedAt:    generatedAt,
	}, nil
}

// buildMultipartyDocument assembles ProofDocument for an N-vendor multiparty contract.
// Returns ErrInvalidTransition if the contract is not ACTIVE.
func (s *ProofService) buildMultipartyDocument(
	ctx context.Context,
	contractID uuid.UUID,
	chainHead string,
	generatedAt time.Time,
) (*domain.ProofDocument, error) {
	c, err := s.mpContracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("get multiparty contract %s: %w", contractID, err)
	}

	if c.Status != domain.MultipartyContractStatusActive {
		return nil, fmt.Errorf("%w: multiparty contract %s is not ACTIVE (status=%s)",
			domain.ErrInvalidTransition, contractID, c.Status)
	}

	mpSigs, err := s.mpSigs.ListByContractVersion(ctx, contractID, c.Version)
	if err != nil {
		return nil, fmt.Errorf("list multiparty signatures for %s v%d: %w", contractID, c.Version, err)
	}

	// Build a vendor→role label map from active parties.
	parties, err := s.mpParties.ListActiveByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list active parties for proof %s: %w", contractID, err)
	}

	vendorRole := make(map[uuid.UUID]string, len(parties))
	for _, p := range parties {
		if p.RoleID != nil {
			vendorRole[p.VendorUserID] = p.RoleID.String()
		} else {
			vendorRole[p.VendorUserID] = "VENDOR"
		}
	}

	entries := make([]domain.ProofSignerEntry, 0, len(mpSigs))
	for _, sig := range mpSigs {
		role := vendorRole[sig.SignerUserID]
		if role == "" {
			role = "VENDOR"
		}

		entries = append(entries, domain.ProofSignerEntry{
			UserID:   sig.SignerUserID,
			Role:     role,
			SignedAt: sig.SignedAt.UTC(),
		})
	}

	// Multiparty contracts don't have a separate title field; derive from tender ID.
	title := fmt.Sprintf("Multiparty Contract (Tender %s)", c.TenderID)

	return &domain.ProofDocument{
		ContractID:     contractID,
		ContractKind:   domain.ContractKindMultiparty,
		Title:          title,
		TermsSummary:   "",
		Version:        c.Version,
		Signers:        entries,
		AuditChainHead: chainHead,
		GeneratedAt:    generatedAt,
	}, nil
}
