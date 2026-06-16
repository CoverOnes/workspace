package service_test

// Shared test constants for the service package tests. Extracted to avoid goconst
// lint violations across the multiple test files in this package that share the
// same literal values. ("TWD" is already defined as testCurrencyTWD in
// multiparty_integration_test.go and is reused rather than redeclared.)
const (
	testKeyListingID        = "listingId"
	testKeyAwardBidID       = "awardBidId"
	testKeyClientUserID     = "clientUserId"
	testKeyFreelancerUserID = "freelancerUserId"
	testKeyAmount           = "amount"
	testKeyCurrency         = "currency"
	testKeyTitle            = "title"
	testKeyTerms            = "terms"

	testValTitle         = "Title"
	testValTestMilestone = "Test Milestone"
	testValDesc          = "desc"
)
