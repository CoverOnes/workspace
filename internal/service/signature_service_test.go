package service_test

import (
	"context"
	"testing"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSignatures(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	t.Run("party can list signatures", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		svc := service.NewSignatureService(cs, ss)

		c := makeContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
		require.NoError(t, cs.Create(context.Background(), c))

		sigs, err := svc.ListSignatures(context.Background(), c.ID, clientID)
		require.NoError(t, err)
		assert.Empty(t, sigs)
	})

	t.Run("non-party gets 404", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		svc := service.NewSignatureService(cs, ss)

		c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c))

		_, err := svc.ListSignatures(context.Background(), c.ID, thirdPartyID)
		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("non-existent contract returns error", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		svc := service.NewSignatureService(cs, ss)

		_, err := svc.ListSignatures(context.Background(), uuid.New(), clientID)
		require.ErrorIs(t, err, domain.ErrContractNotFound)
	})
}
