// Package domain contains core domain types and sentinel errors for the workspace service.
package domain

import "errors"

// Sentinel errors for the workspace domain.
var (
	ErrNotFound          = errors.New("not found")
	ErrUnauthorized      = errors.New("unauthorized")
	ErrForbidden         = errors.New("forbidden")
	ErrValidation        = errors.New("validation error")
	ErrConflict          = errors.New("conflict")
	ErrKYCTierRequired   = errors.New("kyc tier required")
	ErrContractNotFound  = errors.New("contract not found")
	ErrSignatureNotFound = errors.New("signature not found")
	ErrTaskNotFound      = errors.New("task not found")
	ErrWorklogNotFound   = errors.New("worklog not found")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrHashMismatch      = errors.New("signed content hash does not match current contract hash")
	ErrNotParty          = errors.New("caller is not a party to this contract")
	ErrAlreadySigned     = errors.New("party has already signed this version")

	// Multiparty contract sentinel errors.
	ErrMultipartyContractNotFound = errors.New("multi-party contract not found")
	ErrShareSumNotFull            = errors.New("sum of active party share_bps must equal 10000")
	ErrStaleVersion               = errors.New("signature version does not match current contract version")
)
