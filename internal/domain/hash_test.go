package domain_test

import (
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalContractDigest(t *testing.T) {
	const (
		contractID   = "550e8400-e29b-41d4-a716-446655440000"
		clientID     = "11111111-1111-1111-1111-111111111111"
		freelancerID = "22222222-2222-2222-2222-222222222222"
		title        = "Test Contract"
		terms        = "These are the terms."
		amount       = "1000.00"
		currency     = "TWD"
	)

	t.Run("deterministic: same inputs produce same hash", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		assert.Equal(t, h1, h2, "hash must be deterministic")
	})

	t.Run("different contractID produces different hash", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest("different-id-000", clientID, freelancerID, title, terms, amount, currency, 1)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("different clientUserID produces different hash (party binding)", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, "99999999-9999-9999-9999-999999999999", freelancerID, title, terms, amount, currency, 1)
		assert.NotEqual(t, h1, h2, "swapping the client must change the hash")
	})

	t.Run("different freelancerUserID produces different hash (party binding)", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, "99999999-9999-9999-9999-999999999999", title, terms, amount, currency, 1)
		assert.NotEqual(t, h1, h2, "swapping the freelancer must change the hash")
	})

	t.Run("swapping client and freelancer produces different hash", func(t *testing.T) {
		// Binding the parties positionally: (client=A, freelancer=B) must differ
		// from (client=B, freelancer=A) — a role swap is a different signed document.
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, freelancerID, clientID, title, terms, amount, currency, 1)
		assert.NotEqual(t, h1, h2, "client/freelancer role swap must change the hash")
	})

	t.Run("different version produces different hash", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 2)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("different terms produces different hash", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, "altered terms!", amount, currency, 1)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("field boundary: title+terms permutation differs", func(t *testing.T) {
		// title="ab", terms="c" vs title="a", terms="bc"
		// Length-prefixed framing ensures these differ.
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, "ab", "c", amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, "a", "bc", amount, currency, 1)
		assert.NotEqual(t, h1, h2, "length-prefixed framing must prevent field-boundary ambiguity")
	})

	t.Run("output is 64-char hex string (SHA-256)", func(t *testing.T) {
		h := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, amount, currency, 1)
		require.Len(t, h, 64)

		for _, c := range h {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'), "must be lowercase hex")
		}
	})

	t.Run("NFC normalization: composed and decomposed unicode produce same hash", func(t *testing.T) {
		// 'é' can be represented as U+00E9 (composed NFC) or U+0065 U+0301 (decomposed NFD).
		nfc := "café" // café - NFC
		nfd := "café" // café - NFD (e + combining acute)
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, nfc, terms, amount, currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, nfd, terms, amount, currency, 1)
		assert.Equal(t, h1, h2, "NFC normalization must produce same hash for equivalent unicode")
	})

	t.Run("different amount produces different hash", func(t *testing.T) {
		h1 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, "1000.00", currency, 1)
		h2 := domain.CanonicalContractDigest(contractID, clientID, freelancerID, title, terms, "2000.00", currency, 1)
		assert.NotEqual(t, h1, h2)
	})
}
