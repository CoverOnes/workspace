package service

import (
	"context"
	"encoding/json"
	"fmt"

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
// Parameters:
//   - ctx: caller context with localHandlerTimeout applied by the poller.
//   - svc: the ProofService instance captured at server startup.
//   - entry: the outbox entry carrying a JSON-encoded ProofGenerationPayload.
//
// Returns a non-nil error to cause the entry to be retried with exponential backoff.
func HandleProofOutboxEntry(ctx context.Context, svc *ProofService, entry *domain.OutboxEntry) error {
	var payload ProofGenerationPayload

	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		// Malformed payload: returning an error would retry forever. Log and return nil
		// so the entry is marked published and not retried (un-parseable = programmer error).
		return fmt.Errorf("proof outbox handler: unmarshal payload: %w", err)
	}

	if payload.ContractID == uuid.Nil {
		return fmt.Errorf("proof outbox handler: contractId is nil in entry %s", entry.ID)
	}

	kind := domain.ContractKind(payload.Kind)
	if kind != domain.ContractKindBilateral && kind != domain.ContractKindMultiparty {
		return fmt.Errorf("proof outbox handler: unknown contract kind %q in entry %s", payload.Kind, entry.ID)
	}

	if _, err := svc.GenerateAndStore(ctx, payload.ContractID, kind); err != nil {
		return fmt.Errorf("proof outbox handler: generate and store for contract %s: %w", payload.ContractID, err)
	}

	return nil
}
