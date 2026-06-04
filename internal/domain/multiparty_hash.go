package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// MultipartyRosterEntry is one party's data used in the canonical digest.
// The roster is sorted by VendorUserID before hashing so that insert order
// does not influence the digest (determinism invariant).
type MultipartyRosterEntry struct {
	VendorUserID uuid.UUID
	ShareBps     int
}

// CanonicalMultipartyDigest produces a deterministic, version-pinned SHA-256
// over the SORTED roster of parties + their share_bps + the contract version +
// the tender ID.
//
// Canonicalization rules:
//   - Roster is sorted ascending by VendorUserID string representation before hashing.
//     This guarantees the same digest regardless of the order in which parties were
//     inserted (determinism invariant).
//   - Each party entry is length-prefixed: "<len>:<vendor_user_id>:<share_bps>".
//   - The full canonical string is:
//     "<len>:<tender_id>|<version>|N:<entry0>;<entry1>;...;<entryN-1>"
//     where N is the number of parties and each entry is the length-prefixed form.
//   - Any change to TenderID, Version, roster membership, or share_bps produces
//     a different digest.
//
// This function is the ONLY authoritative source of the content_hash for
// multi-party contracts. A signer MUST sign THIS digest, not raw terms text.
func CanonicalMultipartyDigest(tenderID uuid.UUID, version int, roster []MultipartyRosterEntry) string {
	// Sort by VendorUserID to achieve determinism regardless of insert order.
	sorted := make([]MultipartyRosterEntry, len(roster))
	copy(sorted, roster)

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].VendorUserID.String() < sorted[j].VendorUserID.String()
	})

	tenderStr := tenderID.String()

	// Build length-prefixed entry strings.
	entries := make([]string, len(sorted))
	for i, e := range sorted {
		vendorStr := e.VendorUserID.String()
		entries[i] = fmt.Sprintf("%d:%s:%d", len(vendorStr), vendorStr, e.ShareBps)
	}

	// Compose canonical string.
	// Format: "<len(tenderStr)>:<tenderStr>|<version>|<N>:<entry0>;<entry1>;..."
	var entriesPart string
	for i, e := range entries {
		if i > 0 {
			entriesPart += ";"
		}

		entriesPart += e
	}

	canonical := fmt.Sprintf(
		"%d:%s|%d:%d|%d:%s",
		len(tenderStr), tenderStr,
		intLen(version), version,
		len(entriesPart), entriesPart,
	)

	sum := sha256.Sum256([]byte(canonical))

	return hex.EncodeToString(sum[:])
}
