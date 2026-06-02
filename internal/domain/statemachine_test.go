package domain_test

import (
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestValidContractTransition(t *testing.T) {
	tests := []struct {
		name  string
		from  domain.ContractStatus
		to    domain.ContractStatus
		valid bool
	}{
		// DRAFT transitions
		{name: "DRAFT->PENDING_SIGNATURE valid", from: domain.ContractStatusDraft, to: domain.ContractStatusPendingSignature, valid: true},
		{name: "DRAFT->CANCELED valid", from: domain.ContractStatusDraft, to: domain.ContractStatusCanceled, valid: true},
		{name: "DRAFT->ACTIVE invalid", from: domain.ContractStatusDraft, to: domain.ContractStatusActive, valid: false},
		{name: "DRAFT->COMPLETED invalid", from: domain.ContractStatusDraft, to: domain.ContractStatusCompleted, valid: false},
		{name: "DRAFT->SIGNED invalid", from: domain.ContractStatusDraft, to: domain.ContractStatusSigned, valid: false},
		{name: "DRAFT->DRAFT invalid (same state)", from: domain.ContractStatusDraft, to: domain.ContractStatusDraft, valid: false},

		// PENDING_SIGNATURE transitions
		{name: "PENDING->DRAFT valid (terms edit)", from: domain.ContractStatusPendingSignature, to: domain.ContractStatusDraft, valid: true},
		{name: "PENDING->SIGNED valid", from: domain.ContractStatusPendingSignature, to: domain.ContractStatusSigned, valid: true},
		{name: "PENDING->CANCELED valid", from: domain.ContractStatusPendingSignature, to: domain.ContractStatusCanceled, valid: true},
		{name: "PENDING->ACTIVE invalid", from: domain.ContractStatusPendingSignature, to: domain.ContractStatusActive, valid: false},
		{name: "PENDING->COMPLETED invalid", from: domain.ContractStatusPendingSignature, to: domain.ContractStatusCompleted, valid: false},

		// SIGNED transitions (transient)
		{name: "SIGNED->ACTIVE valid", from: domain.ContractStatusSigned, to: domain.ContractStatusActive, valid: true},
		{name: "SIGNED->CANCELED valid", from: domain.ContractStatusSigned, to: domain.ContractStatusCanceled, valid: true},
		{name: "SIGNED->DRAFT invalid", from: domain.ContractStatusSigned, to: domain.ContractStatusDraft, valid: false},
		{name: "SIGNED->COMPLETED invalid", from: domain.ContractStatusSigned, to: domain.ContractStatusCompleted, valid: false},

		// ACTIVE transitions
		{name: "ACTIVE->COMPLETED valid", from: domain.ContractStatusActive, to: domain.ContractStatusCompleted, valid: true},
		{name: "ACTIVE->CANCELED valid", from: domain.ContractStatusActive, to: domain.ContractStatusCanceled, valid: true},
		{name: "ACTIVE->DRAFT invalid", from: domain.ContractStatusActive, to: domain.ContractStatusDraft, valid: false},
		{name: "ACTIVE->SIGNED invalid", from: domain.ContractStatusActive, to: domain.ContractStatusSigned, valid: false},

		// Terminal states admit no transitions
		{name: "COMPLETED->anything invalid", from: domain.ContractStatusCompleted, to: domain.ContractStatusActive, valid: false},
		{name: "CANCELED->anything invalid", from: domain.ContractStatusCanceled, to: domain.ContractStatusDraft, valid: false},
		{
			name:  "COMPLETED->COMPLETED invalid (same state)",
			from:  domain.ContractStatusCompleted,
			to:    domain.ContractStatusCompleted,
			valid: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.ValidContractTransition(tc.from, tc.to)
			assert.Equal(t, tc.valid, got)
		})
	}
}

func TestIsTerminalContractStatus(t *testing.T) {
	tests := []struct {
		status   domain.ContractStatus
		terminal bool
	}{
		{domain.ContractStatusCompleted, true},
		{domain.ContractStatusCanceled, true},
		{domain.ContractStatusDraft, false},
		{domain.ContractStatusPendingSignature, false},
		{domain.ContractStatusSigned, false},
		{domain.ContractStatusActive, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.terminal, domain.IsTerminalContractStatus(tc.status))
		})
	}
}

func TestIsEditableContractStatus(t *testing.T) {
	tests := []struct {
		status   domain.ContractStatus
		editable bool
	}{
		{domain.ContractStatusDraft, true},
		{domain.ContractStatusPendingSignature, true},
		{domain.ContractStatusSigned, false},
		{domain.ContractStatusActive, false},
		{domain.ContractStatusCompleted, false},
		{domain.ContractStatusCanceled, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.editable, domain.IsEditableContractStatus(tc.status))
		})
	}
}
