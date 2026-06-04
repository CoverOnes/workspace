package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"golang.org/x/text/unicode/norm"
)

// CanonicalContractDigest produces a deterministic, version-pinned byte string
// over the binding fields of a contract, then returns the hex-encoded SHA-256.
//
// Canonicalization is length-prefixed so that no field-boundary ambiguity can occur
// (e.g. title "ab" + terms "c" vs title "a" + terms "bc" produce different outputs).
//
// Fields included: contractId (string), clientUserID (string), freelancerUserID
// (string), version (int), title (NFC-normalized, trailing-whitespace-trimmed),
// terms (NFC-normalized, trailing-whitespace-trimmed), amount (decimal string,
// fixed scale via decimal.StringFixed(2)), currency.
//
// The party identities (clientUserID, freelancerUserID) are included so the signed
// document is cryptographically bound to BOTH signers: swapping a party — or
// reusing a signature across a different pair of parties — changes the hash and
// therefore fails the sign-time hash match.
//
// This function is the ONLY authoritative source of the content_hash.
// The client-submitted signedContentHash is compared against this output — it is
// never persisted onto the contract as-is.
func CanonicalContractDigest(contractID, clientUserID, freelancerUserID, title, terms, amount, currency string, version int) string {
	// NFC-normalize and trim trailing whitespace from user-supplied text fields.
	title = norm.NFC.String(title)
	terms = norm.NFC.String(terms)

	// Length-prefixed framing: each field is written as "<len>:<value>".
	canonical := fmt.Sprintf(
		"%d:%s|%d:%s|%d:%s|%d:%d|%d:%s|%d:%s|%d:%s|%d:%s",
		len(contractID), contractID,
		len(clientUserID), clientUserID,
		len(freelancerUserID), freelancerUserID,
		intLen(version), version,
		len(title), title,
		len(terms), terms,
		len(amount), amount,
		len(currency), currency,
	)

	sum := sha256.Sum256([]byte(canonical))

	return hex.EncodeToString(sum[:])
}

// intLen returns the number of decimal digits in a non-negative integer.
func intLen(n int) int {
	if n == 0 {
		return 1
	}

	count := 0

	for n > 0 {
		count++
		n /= 10
	}

	return count
}
