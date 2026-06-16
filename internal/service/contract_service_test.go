package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type fakeContractStore struct {
	contracts map[uuid.UUID]*domain.Contract
}

func newFakeContractStore() *fakeContractStore {
	return &fakeContractStore{contracts: make(map[uuid.UUID]*domain.Contract)}
}

func (f *fakeContractStore) Create(_ context.Context, c *domain.Contract) error {
	if _, exists := f.contracts[c.ID]; exists {
		return domain.ErrConflict
	}

	f.contracts[c.ID] = c

	return nil
}

func (f *fakeContractStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Contract, error) {
	c, ok := f.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}

	return c, nil
}

func (f *fakeContractStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Contract, error) {
	return f.GetByID(ctx, id)
}

func (f *fakeContractStore) ListByParty(_ context.Context, filter store.ContractFilter) ([]*domain.Contract, error) {
	var result []*domain.Contract

	for _, c := range f.contracts {
		if c.ClientUserID == filter.PartyUserID || c.FreelancerUserID == filter.PartyUserID {
			result = append(result, c)
		}
	}

	return result, nil
}

func (f *fakeContractStore) Update(_ context.Context, c *domain.Contract) error {
	if _, ok := f.contracts[c.ID]; !ok {
		return domain.ErrContractNotFound
	}

	f.contracts[c.ID] = c

	return nil
}

type fakeSignatureStore struct {
	sigs map[uuid.UUID][]*domain.Signature
}

func newFakeSignatureStore() *fakeSignatureStore {
	return &fakeSignatureStore{sigs: make(map[uuid.UUID][]*domain.Signature)}
}

func (f *fakeSignatureStore) Create(_ context.Context, s *domain.Signature) error {
	f.sigs[s.ContractID] = append(f.sigs[s.ContractID], s)

	return nil
}

func (f *fakeSignatureStore) ListByContract(_ context.Context, contractID uuid.UUID) ([]*domain.Signature, error) {
	return f.sigs[contractID], nil
}

func (f *fakeSignatureStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Signature, error) {
	for _, sigs := range f.sigs {
		for _, s := range sigs {
			if s.ID == id {
				return s, nil
			}
		}
	}

	return nil, domain.ErrSignatureNotFound
}

func (f *fakeSignatureStore) CountValidSignatures(_ context.Context, contractID uuid.UUID, version int, contentHash string) (int, error) {
	roles := make(map[domain.SignerRole]bool)

	for _, s := range f.sigs[contractID] {
		if s.ContractVersion == version && s.SignedContentHash == contentHash {
			roles[s.SignerRole] = true
		}
	}

	return len(roles), nil
}

func (f *fakeSignatureStore) SetFileID(_ context.Context, id, fileID uuid.UUID) error {
	for _, sigs := range f.sigs {
		for _, s := range sigs {
			if s.ID == id {
				s.FileID = &fileID

				return nil
			}
		}
	}

	return domain.ErrSignatureNotFound
}

type fakeTxManager struct {
	contracts store.ContractStore
	sigs      store.SignatureStore
	outbox    store.OutboxStore
}

func (m *fakeTxManager) WithTx(
	ctx context.Context,
	fn func(ctx context.Context, c store.ContractStore, s store.SignatureStore, o store.OutboxStore) error,
) error {
	return fn(ctx, m.contracts, m.sigs, m.outbox)
}

// noopOutboxStore is a test double for store.OutboxStore that discards all Enqueue calls.
type noopOutboxStore struct{}

func (*noopOutboxStore) Enqueue(_ context.Context, _ *store.OutboxEnqueueInput) error { return nil }
func (*noopOutboxStore) FetchPending(_ context.Context, _ int) ([]*domain.OutboxEntry, error) {
	return nil, nil
}
func (*noopOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (*noopOutboxStore) RecordFailure(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}

func (*noopOutboxStore) DeleteOldPublished(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (*noopOutboxStore) CountStalePending(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// spyOutboxStore records Enqueue calls for assertion in unit tests.
type spyOutboxStore struct {
	enqueued []*store.OutboxEnqueueInput
}

func (s *spyOutboxStore) Enqueue(_ context.Context, in *store.OutboxEnqueueInput) error {
	s.enqueued = append(s.enqueued, in)
	return nil
}

func (s *spyOutboxStore) FetchPending(_ context.Context, _ int) ([]*domain.OutboxEntry, error) {
	return nil, nil
}
func (s *spyOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (s *spyOutboxStore) RecordFailure(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}

func (s *spyOutboxStore) DeleteOldPublished(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (s *spyOutboxStore) CountStalePending(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

type fakePublisher struct {
	published int
}

func (p *fakePublisher) PublishContractActivated(_ context.Context, _ *domain.ContractActivatedEvent) error {
	p.published++

	return nil
}

func (p *fakePublisher) PublishMultipartyContractActivated(_ context.Context, _ *domain.MultipartyContractActivatedEvent) error {
	return nil
}

func (p *fakePublisher) PublishMultipartyContractCompleted(_ context.Context, _ *domain.MultipartyContractCompletedEvent) error {
	return nil
}

func (p *fakePublisher) PublishMultipartyContractAddendumCreated(
	_ context.Context,
	_ *domain.MultipartyContractAddendumCreatedEvent,
) error {
	return nil
}

func (p *fakePublisher) PublishMultipartyContractReSigned(_ context.Context, _ *domain.MultipartyContractReSignedEvent) error {
	return nil
}

// --- helpers ---

func makeContract(clientID, freelancerID uuid.UUID, status domain.ContractStatus) *domain.Contract {
	now := time.Now().UTC()
	amount := decimal.NewFromInt(1000)
	cid := uuid.New()
	hash := domain.CanonicalContractDigest(
		cid.String(), clientID.String(), freelancerID.String(),
		testValTitle, "Terms", amount.StringFixed(2), testCurrencyTWD, 1,
	)

	return &domain.Contract{
		ID:               cid,
		ListingID:        uuid.New(),
		AcceptedBidID:    uuid.New(),
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Title:            testValTitle,
		Terms:            "Terms",
		Amount:           amount,
		Currency:         testCurrencyTWD,
		ContentHash:      hash,
		Version:          1,
		Status:           status,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// --- tests ---

func TestCreateContract(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	tests := []struct {
		name    string
		in      *service.CreateContractInput
		wantErr bool
		errIs   error
	}{
		{
			name: "happy path creates DRAFT contract",
			in: &service.CreateContractInput{
				ClientUserID:     clientID,
				ListingID:        uuid.New(),
				AcceptedBidID:    uuid.New(),
				FreelancerUserID: freelancerID,
				Title:            "Valid Title",
				Terms:            "Valid terms.",
				Amount:           decimal.NewFromInt(1000),
				Currency:         testCurrencyTWD,
			},
			wantErr: false,
		},
		{
			name: "empty title rejected",
			in: &service.CreateContractInput{
				ClientUserID:     clientID,
				FreelancerUserID: freelancerID,
				Title:            "",
				Terms:            testKeyTerms,
				Amount:           decimal.NewFromInt(100),
				Currency:         testCurrencyTWD,
			},
			wantErr: true,
			errIs:   domain.ErrValidation,
		},
		{
			name: "zero amount rejected",
			in: &service.CreateContractInput{
				ClientUserID:     clientID,
				FreelancerUserID: freelancerID,
				Title:            testValTitle,
				Terms:            testKeyTerms,
				Amount:           decimal.Zero,
				Currency:         testCurrencyTWD,
			},
			wantErr: true,
			errIs:   domain.ErrValidation,
		},
		{
			name: "client == freelancer rejected",
			in: &service.CreateContractInput{
				ClientUserID:     clientID,
				FreelancerUserID: clientID,
				Title:            testValTitle,
				Terms:            testKeyTerms,
				Amount:           decimal.NewFromInt(500),
				Currency:         testCurrencyTWD,
			},
			wantErr: true,
			errIs:   domain.ErrValidation,
		},
		{
			name: "invalid currency rejected",
			in: &service.CreateContractInput{
				ClientUserID:     clientID,
				FreelancerUserID: freelancerID,
				Title:            testValTitle,
				Terms:            testKeyTerms,
				Amount:           decimal.NewFromInt(100),
				Currency:         "TWDD",
			},
			wantErr: true,
			errIs:   domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeContractStore()
			ss := newFakeSignatureStore()
			tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
			pub := &fakePublisher{}

			svc := service.NewContractService(cs, ss, tx, pub, nil)

			result, err := svc.CreateContract(context.Background(), tc.in)

			if tc.wantErr {
				require.Error(t, err)

				if tc.errIs != nil {
					require.ErrorIs(t, err, tc.errIs)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, domain.ContractStatusDraft, result.Status)
			assert.Equal(t, 1, result.Version)
			assert.NotEmpty(t, result.ContentHash)
			assert.Equal(t, tc.in.ClientUserID, result.ClientUserID)
		})
	}
}

func TestGetContract_IDORProtection(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	cs := newFakeContractStore()
	ss := newFakeSignatureStore()
	tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
	pub := &fakePublisher{}
	svc := service.NewContractService(cs, ss, tx, pub, nil)

	c := makeContract(clientID, freelancerID, domain.ContractStatusDraft)
	require.NoError(t, cs.Create(context.Background(), c))

	t.Run("client can get contract", func(t *testing.T) {
		result, err := svc.GetContract(context.Background(), c.ID, clientID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, result.ID)
	})

	t.Run("freelancer can get contract", func(t *testing.T) {
		result, err := svc.GetContract(context.Background(), c.ID, freelancerID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, result.ID)
	})

	t.Run("non-party gets 404 (not 403)", func(t *testing.T) {
		_, err := svc.GetContract(context.Background(), c.ID, thirdPartyID)
		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("non-existent contract returns error", func(t *testing.T) {
		_, err := svc.GetContract(context.Background(), uuid.New(), clientID)
		require.ErrorIs(t, err, domain.ErrContractNotFound)
	})
}

func TestSubmitContract(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	tests := []struct {
		name     string
		status   domain.ContractStatus
		callerID uuid.UUID
		wantErr  bool
		errIs    error
	}{
		{
			name:     "client submits DRAFT -> PENDING_SIGNATURE",
			status:   domain.ContractStatusDraft,
			callerID: clientID,
			wantErr:  false,
		},
		{
			name:     "freelancer cannot submit (not client)",
			status:   domain.ContractStatusDraft,
			callerID: freelancerID,
			wantErr:  true,
			errIs:    domain.ErrNotFound,
		},
		{
			name:     "cannot submit ACTIVE contract",
			status:   domain.ContractStatusActive,
			callerID: clientID,
			wantErr:  true,
			errIs:    domain.ErrInvalidTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeContractStore()
			ss := newFakeSignatureStore()
			tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
			pub := &fakePublisher{}
			svc := service.NewContractService(cs, ss, tx, pub, nil)

			c := makeContract(clientID, freelancerID, tc.status)
			require.NoError(t, cs.Create(context.Background(), c))

			result, err := svc.SubmitContract(context.Background(), c.ID, tc.callerID)

			if tc.wantErr {
				require.Error(t, err)

				if tc.errIs != nil {
					require.ErrorIs(t, err, tc.errIs)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, domain.ContractStatusPendingSignature, result.Status)
		})
	}
}

func TestSignContract_DualSign(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	t.Run("first sign leaves PENDING_SIGNATURE", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
		pub := &fakePublisher{}
		svc := service.NewContractService(cs, ss, tx, pub, nil)

		c := makeContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
		require.NoError(t, cs.Create(context.Background(), c))

		result, err := svc.SignContract(context.Background(), service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          clientID,
			SignedContentHash: c.ContentHash,
		})

		require.NoError(t, err)
		assert.Equal(t, domain.ContractStatusPendingSignature, result.Status)
		assert.Equal(t, 0, pub.published)
	})

	t.Run("dual sign activates contract and enqueues outbox event", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		spy := &spyOutboxStore{}
		tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: spy}
		pub := &fakePublisher{}
		svc := service.NewContractService(cs, ss, tx, pub, nil)

		c := makeContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
		require.NoError(t, cs.Create(context.Background(), c))

		_, err := svc.SignContract(context.Background(), service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          clientID,
			SignedContentHash: c.ContentHash,
		})
		require.NoError(t, err)

		result, err := svc.SignContract(context.Background(), service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          freelancerID,
			SignedContentHash: c.ContentHash,
		})

		require.NoError(t, err)
		assert.Equal(t, domain.ContractStatusActive, result.Status)
		assert.NotNil(t, result.ActivatedAt)
		// Publisher is no longer called directly; events are enqueued in the outbox atomically.
		assert.Equal(t, 0, pub.published)
		// Dual-sign activation enqueues two outbox entries:
		//   [0] contract_activated  — notifies downstream consumers via Redis relay
		//   [1] proof_generation_required — handled in-process by the outbox poller
		require.Len(t, spy.enqueued, 2, "expected 2 outbox entries on dual-sign activation (contract_activated + proof_generation_required)")
		assert.Equal(t, "contract", spy.enqueued[0].AggregateType)
		assert.Equal(t, c.ID, spy.enqueued[0].AggregateID)
		assert.Equal(t, "workspace.contract_activated", spy.enqueued[0].Channel)
		assert.Equal(t, "contract", spy.enqueued[1].AggregateType)
		assert.Equal(t, c.ID, spy.enqueued[1].AggregateID)
		assert.Equal(t, service.ChannelProofGenerationRequired, spy.enqueued[1].Channel)
	})

	t.Run("hash mismatch returns ErrHashMismatch", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
		pub := &fakePublisher{}
		svc := service.NewContractService(cs, ss, tx, pub, nil)

		c := makeContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
		require.NoError(t, cs.Create(context.Background(), c))

		_, err := svc.SignContract(context.Background(), service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          clientID,
			SignedContentHash: "wrong_hash",
		})

		require.ErrorIs(t, err, domain.ErrHashMismatch)
	})

	t.Run("non-party cannot sign", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
		pub := &fakePublisher{}
		svc := service.NewContractService(cs, ss, tx, pub, nil)

		c := makeContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
		require.NoError(t, cs.Create(context.Background(), c))

		_, err := svc.SignContract(context.Background(), service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          uuid.New(),
			SignedContentHash: c.ContentHash,
		})

		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("body change invalidates prior signatures (new version)", func(t *testing.T) {
		cs := newFakeContractStore()
		ss := newFakeSignatureStore()
		tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
		pub := &fakePublisher{}
		svc := service.NewContractService(cs, ss, tx, pub, nil)

		c := makeContract(clientID, freelancerID, domain.ContractStatusPendingSignature)
		require.NoError(t, cs.Create(context.Background(), c))

		// Save original hash before mutation (c is a pointer; fake store shares it).
		originalHash := c.ContentHash

		// Client signs v1.
		_, err := svc.SignContract(context.Background(), service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          clientID,
			SignedContentHash: c.ContentHash,
		})
		require.NoError(t, err)

		// Freelancer edits terms -> version bumps to 2, old hash invalidated.
		newTerms := "revised terms"
		_, err = svc.PatchContract(context.Background(), service.PatchContractInput{
			ID:       c.ID,
			CallerID: freelancerID,
			Terms:    &newTerms,
		})
		require.NoError(t, err)

		// Re-fetch the contract to get new hash/version.
		updated := cs.contracts[c.ID]
		assert.Equal(t, 2, updated.Version)
		assert.NotEqual(t, originalHash, updated.ContentHash)

		// Now contract is DRAFT again (submit required before signing v2).
		assert.Equal(t, domain.ContractStatusDraft, updated.Status)
	})
}

// TestConcurrentMutationRejection verifies that state-changing contract mutations
// use GetByIDForUpdate and re-validate state after the lock is acquired, so that a
// concurrent writer cannot drive an illegal transition or silently clobber.
//
// The fake store's fakeTxManager calls the fn with the same stores, simulating the
// transactional read-then-write pattern. We verify that mutations fail when the
// contract is already in a terminal/wrong state at the moment the lock is read.
func TestConcurrentMutationRejection(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	tests := []struct {
		name    string
		setup   func(cs *fakeContractStore, c *domain.Contract)
		mutate  func(svc *service.ContractService, id uuid.UUID) error
		wantErr error
	}{
		{
			name: "cancel after complete is rejected (second cancel on terminal state)",
			setup: func(_ *fakeContractStore, c *domain.Contract) {
				// Drive the contract to COMPLETED directly, simulating a concurrent CompleteContract.
				c.Status = domain.ContractStatusCompleted
			},
			mutate: func(svc *service.ContractService, id uuid.UUID) error {
				_, err := svc.CancelContract(context.Background(), id, clientID)

				return err
			},
			wantErr: domain.ErrInvalidTransition,
		},
		{
			name: "patch after submit is rejected when contract moves to ACTIVE",
			setup: func(_ *fakeContractStore, c *domain.Contract) {
				// Simulate concurrent activation — contract is now ACTIVE, not editable.
				c.Status = domain.ContractStatusActive
			},
			mutate: func(svc *service.ContractService, id uuid.UUID) error {
				newTerms := "hacked terms"
				_, err := svc.PatchContract(context.Background(), service.PatchContractInput{
					ID:       id,
					CallerID: clientID,
					Terms:    &newTerms,
				})

				return err
			},
			wantErr: domain.ErrInvalidTransition,
		},
		{
			name: "submit on already-PENDING_SIGNATURE contract is rejected",
			setup: func(_ *fakeContractStore, c *domain.Contract) {
				// Already submitted by a concurrent request.
				c.Status = domain.ContractStatusPendingSignature
			},
			mutate: func(svc *service.ContractService, id uuid.UUID) error {
				_, err := svc.SubmitContract(context.Background(), id, clientID)

				return err
			},
			wantErr: domain.ErrInvalidTransition,
		},
		{
			name: "complete on already-COMPLETED contract is rejected",
			setup: func(_ *fakeContractStore, c *domain.Contract) {
				// Already completed by a concurrent request.
				c.Status = domain.ContractStatusCompleted
			},
			mutate: func(svc *service.ContractService, id uuid.UUID) error {
				_, err := svc.CompleteContract(context.Background(), id, clientID)

				return err
			},
			wantErr: domain.ErrInvalidTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeContractStore()
			ss := newFakeSignatureStore()
			tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
			pub := &fakePublisher{}
			svc := service.NewContractService(cs, ss, tx, pub, nil)

			c := makeContract(clientID, freelancerID, domain.ContractStatusDraft)
			require.NoError(t, cs.Create(context.Background(), c))

			// Apply the "concurrent mutation" that happened while our request was in-flight.
			tc.setup(cs, cs.contracts[c.ID])

			err := tc.mutate(svc, c.ID)
			require.Error(t, err)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestCancelContract(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	tests := []struct {
		name     string
		status   domain.ContractStatus
		callerID uuid.UUID
		wantErr  bool
		errIs    error
	}{
		{name: "client can cancel DRAFT", status: domain.ContractStatusDraft, callerID: clientID},
		{name: "freelancer can cancel DRAFT", status: domain.ContractStatusDraft, callerID: freelancerID},
		{name: "client can cancel PENDING", status: domain.ContractStatusPendingSignature, callerID: clientID},
		{name: "client can cancel ACTIVE", status: domain.ContractStatusActive, callerID: clientID},
		{
			name:     "cannot cancel COMPLETED",
			status:   domain.ContractStatusCompleted,
			callerID: clientID,
			wantErr:  true,
			errIs:    domain.ErrInvalidTransition,
		},
		{
			name:     "non-party gets 404",
			status:   domain.ContractStatusDraft,
			callerID: uuid.New(),
			wantErr:  true,
			errIs:    domain.ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeContractStore()
			ss := newFakeSignatureStore()
			tx := &fakeTxManager{contracts: cs, sigs: ss, outbox: &noopOutboxStore{}}
			pub := &fakePublisher{}
			svc := service.NewContractService(cs, ss, tx, pub, nil)

			c := makeContract(clientID, freelancerID, tc.status)
			require.NoError(t, cs.Create(context.Background(), c))

			result, err := svc.CancelContract(context.Background(), c.ID, tc.callerID)

			if tc.wantErr {
				require.Error(t, err)

				if tc.errIs != nil {
					require.ErrorIs(t, err, tc.errIs)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, domain.ContractStatusCanceled, result.Status)
		})
	}
}
