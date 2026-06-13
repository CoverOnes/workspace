// Package domain contains core domain types and business logic for the workspace service.
//
// # Hash-chain audit log — security scope
//
// AuditEntryDigest produces a tamper-evident hash for each ContractAuditLog entry.
// The chain detects application-layer tampering: row deletion, payload modification,
// reordering, and partial writes. VerifyAuditChain recomputes every entry and checks
// the prev_hash linkage.
//
// Limitation: a DB-privileged attacker who can write arbitrary rows can recompute the
// entire chain (all inputs are stored in the DB). True non-repudiation requires an
// HMAC key outside the database (HSM, external log anchor, or signed checkpoints).
// That is a separate product decision; see GTD backlog.
package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// AuditEntryDigest computes the canonical SHA-256 hash for a contract audit log entry
// using length-prefixed encoding to eliminate boundary ambiguity.
//
// Each field is written as: 8-byte big-endian uint64 length, then the field bytes.
// Fields in order: prevHash, contractID, eventType, actorID, canonicalPayload.
//
// canonicalPayload is json.Marshal of the payload map — Go's encoding/json produces
// lexicographically sorted keys for map[string]any, making the encoding deterministic.
func AuditEntryDigest(prevHash string, contractID uuid.UUID, eventType string, actorID uuid.UUID, payload map[string]any) (string, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal audit payload: %w", err)
	}

	h := sha256.New()
	writeLenPrefixed(h, []byte(prevHash))
	writeLenPrefixed(h, []byte(contractID.String()))
	writeLenPrefixed(h, []byte(eventType))
	writeLenPrefixed(h, []byte(actorID.String()))
	writeLenPrefixed(h, payloadBytes)

	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyAuditChain recomputes the hash for each entry in the chain and returns
// true if the chain is intact, false if any entry's hash does not match its
// recomputed value or the prev_hash linkage is broken.
//
// entries MUST be ordered by seq ASC (oldest first) for the same contract.
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
			return false, fmt.Errorf("recompute hash for entry seq %d: %w", entry.Seq, err)
		}

		if recomputed != entry.Hash {
			return false, nil
		}
	}

	return true, nil
}

// writeLenPrefixed writes a length-prefixed field to w:
//
//	[8-byte big-endian uint64 len][bytes]
//
// Using 8-byte headers prevents boundary confusion between fields of different
// lengths (e.g. eventType "AB" || actorID "CD..." ≠ eventType "ABCD..." || actorID "").
func writeLenPrefixed(w interface{ Write([]byte) (int, error) }, data []byte) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(data)))
	_, _ = w.Write(lenBuf[:])
	_, _ = w.Write(data)
}
