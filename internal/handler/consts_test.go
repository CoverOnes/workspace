package handler_test

// Shared test constants for handler package tests.
// Extracted to avoid goconst lint violations across the multiple test files
// in this package that share the same literal values.
const (
	testCurrencyTWD = "TWD"

	testKeyListingID         = "listingId"
	testKeyFreelancerUserID  = "freelancerUserId"
	testKeyClientUserID      = "clientUserId"
	testKeyAwardBidID        = "awardBidId"
	testKeyAmount            = "amount"
	testKeyCurrency          = "currency"
	testKeySignedContentHash = "signedContentHash"
	testKeyTitle             = "title"

	testErrCodeValidation = "VALIDATION_ERROR"
	testErrCodeNotFound   = "NOT_FOUND"
)
