package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
)

// ChannelProofGenerationRequired is the outbox channel name for the in-process proof
// generation handler. This channel has NO external Redis subscriber — entries are
// dispatched locally by the outbox poller via Handle registration.
// Do not mirror into the events package.
const ChannelProofGenerationRequired = "workspace.proof_generation_required"

// ProofGenerationPayload is the outbox entry payload for ChannelProofGenerationRequired.
// It carries the minimum information needed to invoke GenerateAndStore.
type ProofGenerationPayload struct {
	// ContractID is the UUID of the contract for which a proof PDF should be generated.
	ContractID uuid.UUID `json:"contractId"`
	// Kind is the contract kind ("bilateral" or "multiparty").
	Kind string `json:"kind"`
}

// HandleProofOutboxEntry is the local outbox handler for ChannelProofGenerationRequired.
// It is registered with the outbox.Poller via Handle(ChannelProofGenerationRequired, ...).
//
// At-least-once delivery is safe because GenerateAndStore is version-aware and idempotent:
// same version → skip (return existing); older version → supersede (overwrite) in place.
//
// Error handling policy:
//   - Programmer-error conditions (malformed JSON, nil ContractID, unknown kind): these
//     cannot be corrected by retrying, so the handler logs a warning and returns nil.
//     Returning nil causes the poller to call MarkPublished and stop retrying.
//   - Transient errors from GenerateAndStore (network, DB, file service unavailable):
//     these are returned as non-nil so the poller records a failure and applies backoff.
//
// Parameters:
//   - ctx: caller context with localHandlerTimeout applied by the poller.
//   - svc: the ProofService instance captured at server startup.
//   - entry: the outbox entry carrying a JSON-encoded ProofGenerationPayload.
func HandleProofOutboxEntry(ctx context.Context, svc *ProofService, entry *domain.OutboxEntry) error {
	var payload ProofGenerationPayload

	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		// Malformed payload: not retryable — programmer error in the enqueue path.
		// Log, mark published (return nil), and move on.
		slog.Warn("proof outbox handler: malformed payload; dropping entry",
			"entry_id", entry.ID, "err", err)

		return nil
	}

	if payload.ContractID == uuid.Nil {
		// Nil contractId is a programmer error — cannot be fixed by retrying.
		slog.Warn("proof outbox handler: contractId is nil; dropping entry", "entry_id", entry.ID)

		return nil
	}

	kind := domain.ContractKind(payload.Kind)
	if kind != domain.ContractKindBilateral && kind != domain.ContractKindMultiparty {
		// Unknown kind is a programmer error — cannot be fixed by retrying.
		slog.Warn("proof outbox handler: unknown contract kind; dropping entry",
			"entry_id", entry.ID, "kind", payload.Kind)

		return nil
	}

	// GenerateAndStore errors (network, DB, file service) are transient — return the error
	// so the poller records a failure and retries with exponential backoff.
	if _, err := svc.GenerateAndStore(ctx, payload.ContractID, kind); err != nil {
		return fmt.Errorf("proof outbox handler: generate and store for contract %s: %w", payload.ContractID, err)
	}

	return nil
}
