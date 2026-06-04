package domain_test

import (
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCanonicalMultipartyDigest_Determinism verifies:
// 1. Same roster in any insert order → identical digest.
// 2. Any roster change (party added/removed, share changed) → different digest.
// 3. Version change → different digest.
// 4. TenderID change → different digest.
func TestCanonicalMultipartyDigest_Determinism(t *testing.T) {
	tenderID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vendorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	vendorB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	vendorC := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	roster := []domain.MultipartyRosterEntry{
		{VendorUserID: vendorA, ShareBps: 5000},
		{VendorUserID: vendorB, ShareBps: 3000},
		{VendorUserID: vendorC, ShareBps: 2000},
	}

	// Reference digest computed with the canonical order.
	ref := domain.CanonicalMultipartyDigest(tenderID, 1, roster)

	require.NotEmpty(t, ref, "digest must be non-empty")
	assert.Len(t, ref, 64, "SHA-256 hex digest must be 64 chars")

	t.Run("same roster in different insert orders produce identical digest", func(t *testing.T) {
		permutations := [][]domain.MultipartyRosterEntry{
			{
				{VendorUserID: vendorB, ShareBps: 3000},
				{VendorUserID: vendorA, ShareBps: 5000},
				{VendorUserID: vendorC, ShareBps: 2000},
			},
			{
				{VendorUserID: vendorC, ShareBps: 2000},
				{VendorUserID: vendorA, ShareBps: 5000},
				{VendorUserID: vendorB, ShareBps: 3000},
			},
			{
				{VendorUserID: vendorC, ShareBps: 2000},
				{VendorUserID: vendorB, ShareBps: 3000},
				{VendorUserID: vendorA, ShareBps: 5000},
			},
			{
				{VendorUserID: vendorB, ShareBps: 3000},
				{VendorUserID: vendorC, ShareBps: 2000},
				{VendorUserID: vendorA, ShareBps: 5000},
			},
		}

		for i, perm := range permutations {
			got := domain.CanonicalMultipartyDigest(tenderID, 1, perm)
			assert.Equal(t, ref, got, "permutation %d must produce the same digest as the canonical order", i)
		}
	})

	t.Run("changing a party's share_bps changes the digest", func(t *testing.T) {
		modified := []domain.MultipartyRosterEntry{
			{VendorUserID: vendorA, ShareBps: 4000}, // changed from 5000
			{VendorUserID: vendorB, ShareBps: 3000},
			{VendorUserID: vendorC, ShareBps: 2000},
		}
		assert.NotEqual(t, ref, domain.CanonicalMultipartyDigest(tenderID, 1, modified),
			"changed share_bps must produce a different digest")
	})

	t.Run("removing a party changes the digest", func(t *testing.T) {
		twoParty := []domain.MultipartyRosterEntry{
			{VendorUserID: vendorA, ShareBps: 7000},
			{VendorUserID: vendorB, ShareBps: 3000},
		}
		assert.NotEqual(t, ref, domain.CanonicalMultipartyDigest(tenderID, 1, twoParty),
			"removing a party must produce a different digest")
	})

	t.Run("adding a party changes the digest", func(t *testing.T) {
		vendorD := uuid.New()
		fourParty := make([]domain.MultipartyRosterEntry, len(roster)+1)
		copy(fourParty, roster)
		fourParty[len(roster)] = domain.MultipartyRosterEntry{VendorUserID: vendorD, ShareBps: 0}
		assert.NotEqual(t, ref, domain.CanonicalMultipartyDigest(tenderID, 1, fourParty),
			"adding a party must produce a different digest")
	})

	t.Run("version bump changes the digest", func(t *testing.T) {
		assert.NotEqual(t, ref, domain.CanonicalMultipartyDigest(tenderID, 2, roster),
			"version change must produce a different digest")
	})

	t.Run("different tenderID changes the digest", func(t *testing.T) {
		otherTender := uuid.New()
		assert.NotEqual(t, ref, domain.CanonicalMultipartyDigest(otherTender, 1, roster),
			"different tenderID must produce a different digest")
	})

	t.Run("empty roster is handled deterministically", func(t *testing.T) {
		d1 := domain.CanonicalMultipartyDigest(tenderID, 1, nil)
		d2 := domain.CanonicalMultipartyDigest(tenderID, 1, []domain.MultipartyRosterEntry{})
		assert.Equal(t, d1, d2, "nil and empty slice must produce identical digest")
		assert.NotEqual(t, ref, d1, "empty roster must differ from non-empty roster")
	})

	t.Run("swapping two vendors' share_bps changes the digest (parties are not interchangeable)", func(t *testing.T) {
		// vendorA gets B's share and vice-versa — same sum, different binding.
		swapped := []domain.MultipartyRosterEntry{
			{VendorUserID: vendorA, ShareBps: 3000}, // A gets B's share
			{VendorUserID: vendorB, ShareBps: 5000}, // B gets A's share
			{VendorUserID: vendorC, ShareBps: 2000},
		}
		assert.NotEqual(t, ref, domain.CanonicalMultipartyDigest(tenderID, 1, swapped),
			"swapping shares between vendors must change the digest even though the total is the same")
	})
}
