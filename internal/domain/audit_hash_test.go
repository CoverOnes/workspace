package domain_test

import (
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditEntryDigest_Deterministic verifies that the same inputs always produce
// the same hash.
func TestAuditEntryDigest_Deterministic(t *testing.T) {
	contractID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	actorID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	payload := map[string]any{"status": "ACTIVE", "amount": "5000.00"}

	h1, err := domain.AuditEntryDigest("prev", contractID, "CONTRACT_ACTIVATED", actorID, payload)
	require.NoError(t, err)

	h2, err := domain.AuditEntryDigest("prev", contractID, "CONTRACT_ACTIVATED", actorID, payload)
	require.NoError(t, err)

	assert.Equal(t, h1, h2, "same inputs must produce same hash")
	assert.Len(t, h1, 64, "SHA-256 hex string must be 64 chars")
}

// TestAuditEntryDigest_DifferentPrevHash verifies that changing prev_hash changes the digest.
func TestAuditEntryDigest_DifferentPrevHash(t *testing.T) {
	contractID := uuid.New()
	actorID := uuid.New()
	payload := map[string]any{}

	h1, err := domain.AuditEntryDigest("prev1", contractID, "EVENT", actorID, payload)
	require.NoError(t, err)

	h2, err := domain.AuditEntryDigest("prev2", contractID, "EVENT", actorID, payload)
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2, "different prev_hash must produce different hash")
}

// TestAuditEntryDigest_LengthPrefixBoundary verifies that the length-prefixed encoding
// prevents boundary confusion between adjacent variable-length fields.
// Without length prefixes, ("AB", "CD") and ("A", "BCD") would produce identical byte
// streams for eventType+actorID; with length prefixes they are distinct.
func TestAuditEntryDigest_LengthPrefixBoundary(t *testing.T) {
	contractID := uuid.New()
	actorID := uuid.New()
	payload := map[string]any{}

	// Two different (prevHash, eventType) pairs whose raw concatenation is identical
	// but whose length-prefixed encoding is different.
	h1, err := domain.AuditEntryDigest("AB", contractID, "CD", actorID, payload)
	require.NoError(t, err)

	h2, err := domain.AuditEntryDigest("A", contractID, "BCD", actorID, payload)
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2, "length-prefixed encoding must distinguish boundary-ambiguous inputs")
}

// TestVerifyAuditChain_IntactChain verifies a correctly-chained sequence of 5 entries.
func TestVerifyAuditChain_IntactChain(t *testing.T) {
	contractID := uuid.New()
	actorID := uuid.New()
	entries := buildChain(t, contractID, actorID, 5)

	ok, err := domain.VerifyAuditChain(entries)
	require.NoError(t, err)
	assert.True(t, ok, "intact 5-entry chain must verify as true")
}

// TestVerifyAuditChain_TamperedHash verifies that altering a single entry's hash
// causes the chain verification to return false.
func TestVerifyAuditChain_TamperedHash(t *testing.T) {
	contractID := uuid.New()
	actorID := uuid.New()
	entries := buildChain(t, contractID, actorID, 5)

	// Tamper: corrupt the hash of entry at index 2.
	entries[2].Hash = "000000000000000000000000000000000000000000000000000000000000dead"

	ok, err := domain.VerifyAuditChain(entries)
	require.NoError(t, err)
	assert.False(t, ok, "tampered hash must fail chain verification")
}

// TestVerifyAuditChain_TamperedPayload verifies that changing a payload value
// causes verification to fail (recomputed hash won't match stored hash).
func TestVerifyAuditChain_TamperedPayload(t *testing.T) {
	contractID := uuid.New()
	actorID := uuid.New()
	entries := buildChain(t, contractID, actorID, 5)

	// Tamper: change a payload value without recomputing the hash.
	entries[1].Payload["amount"] = "9999.00"

	ok, err := domain.VerifyAuditChain(entries)
	require.NoError(t, err)
	assert.False(t, ok, "tampered payload must fail chain verification")
}

// TestVerifyAuditChain_BrokenPrevHashLink verifies that a prev_hash linkage break
// is detected even when the stored hash matches the recomputed one.
func TestVerifyAuditChain_BrokenPrevHashLink(t *testing.T) {
	contractID := uuid.New()
	actorID := uuid.New()
	entries := buildChain(t, contractID, actorID, 3)

	// Tamper: entry[1].PrevHash no longer matches entry[0].Hash.
	entries[1].PrevHash = "cafecafecafe"

	ok, err := domain.VerifyAuditChain(entries)
	require.NoError(t, err)
	assert.False(t, ok, "broken prev_hash link must fail chain verification")
}

// TestVerifyAuditChain_EmptyChain verifies that an empty chain is considered intact.
func TestVerifyAuditChain_EmptyChain(t *testing.T) {
	ok, err := domain.VerifyAuditChain([]*domain.ContractAuditLog{})
	require.NoError(t, err)
	assert.True(t, ok, "empty chain must be considered intact")
}

// buildChain constructs a valid n-entry hash chain for testing.
func buildChain(t *testing.T, contractID, actorID uuid.UUID, n int) []*domain.ContractAuditLog {
	t.Helper()

	base := time.Now().UTC()
	entries := make([]*domain.ContractAuditLog, n)
	prevHash := ""

	for i := range n {
		payload := map[string]any{"seq": i, "amount": "5000.00"}
		h, err := domain.AuditEntryDigest(prevHash, contractID, "TEST_EVENT", actorID, payload)
		require.NoError(t, err)

		entries[i] = &domain.ContractAuditLog{
			Seq:        int64(i + 1),
			ID:         uuid.New(),
			ContractID: contractID,
			EventType:  "TEST_EVENT",
			ActorID:    actorID,
			Payload:    payload,
			PrevHash:   prevHash,
			Hash:       h,
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
		}

		prevHash = h
	}

	return entries
}
