package service

import (
	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
)

// assertParty returns nil if callerID is either the client or freelancer party,
// or ErrNotFound if the caller is not a party.
// ErrNotFound (not ErrForbidden) is used deliberately to prevent resource-existence
// enumeration — non-party access is indistinguishable from a non-existent resource.
func assertParty(c *domain.Contract, callerID uuid.UUID) error {
	if callerID != c.ClientUserID && callerID != c.FreelancerUserID {
		return domain.ErrNotFound
	}

	return nil
}

// assertClientOnly returns nil if callerID is the client party, or ErrNotFound otherwise.
func assertClientOnly(c *domain.Contract, callerID uuid.UUID) error {
	if callerID != c.ClientUserID {
		return domain.ErrNotFound
	}

	return nil
}

// deriveSignerRole returns the SignerRole for callerID given a contract.
// Returns ErrNotParty if callerID is neither party.
func deriveSignerRole(c *domain.Contract, callerID uuid.UUID) (domain.SignerRole, error) {
	switch callerID {
	case c.ClientUserID:
		return domain.SignerRoleClient, nil
	case c.FreelancerUserID:
		return domain.SignerRoleFreelancer, nil
	default:
		return "", domain.ErrNotFound
	}
}
