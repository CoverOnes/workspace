package domain

// ValidContractTransition returns true when a contract can transition from its current
// status to the requested target status.
// Terminal states (COMPLETED, CANCELED) admit no further transitions.
// Mirrors marketplace ValidBidTransition.
func ValidContractTransition(from, to ContractStatus) bool {
	if from == to {
		return false
	}

	switch from {
	case ContractStatusDraft:
		switch to {
		case ContractStatusPendingSignature, ContractStatusCanceled:
			return true
		}

	case ContractStatusPendingSignature:
		// A terms edit while PENDING resets back to DRAFT (version bump).
		switch to {
		case ContractStatusDraft, ContractStatusSigned, ContractStatusCanceled:
			return true
		}

	case ContractStatusSigned:
		// SIGNED is transient — auto-promoted to ACTIVE in the same tx, or canceled.
		switch to {
		case ContractStatusActive, ContractStatusCanceled:
			return true
		}

	case ContractStatusActive:
		switch to {
		case ContractStatusCompleted, ContractStatusCanceled:
			return true
		}
	}

	// COMPLETED and CANCELED are terminal — no further transitions.
	return false
}

// IsTerminalContractStatus reports whether s is a terminal contract state.
func IsTerminalContractStatus(s ContractStatus) bool {
	return s == ContractStatusCompleted || s == ContractStatusCanceled
}

// IsEditableContractStatus reports whether a contract in state s may have its
// terms/title/amount edited (only DRAFT and PENDING_SIGNATURE are editable).
func IsEditableContractStatus(s ContractStatus) bool {
	return s == ContractStatusDraft || s == ContractStatusPendingSignature
}
