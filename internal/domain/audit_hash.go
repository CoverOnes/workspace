package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// AuditEntryDigest computes the canonical SHA-256 hash for a contract audit log entry.
//
// The input is the concatenation:
//
//	prevHash || contractID || eventType || actorID || canonicalPayload
//
// where canonicalPayload is the deterministic JSON encoding of the payload map
// (keys sorted, compact). This ensures the hash is reproducible independent of
// insertion order in the map.
func AuditEntryDigest(prevHash string, contractID uuid.UUID, eventType string, actorID uuid.UUID, payload map[string]any) (string, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal audit payload: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte(contractID.String()))
	h.Write([]byte(eventType))
	h.Write([]byte(actorID.String()))
	h.Write(payloadBytes)

	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyAuditChain recomputes the hash for each entry in the chain and returns
// true if the chain is intact, false if any entry's hash does not match its
// recomputed value or the prev_hash linkage is broken.
//
// entries MUST be ordered by created_at ASC (oldest first) for the same contract.
func VerifyAuditChain(entries []*ContractAuditLog) (bool, error) {
	for i, entry := range entries {
		// Determine the expected prev_hash.
		expectedPrev := ""
		if i > 0 {
			expectedPrev = entries[i-1].Hash
		}

		if entry.PrevHash != expectedPrev {
			return false, nil
		}

		recomputed, err := AuditEntryDigest(entry.PrevHash, entry.ContractID, entry.EventType, entry.ActorID, entry.Payload)
		if err != nil {
			return false, fmt.Errorf("recompute hash for entry %s: %w", entry.ID, err)
		}

		if recomputed != entry.Hash {
			return false, nil
		}
	}

	return true, nil
}
