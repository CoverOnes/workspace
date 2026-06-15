package service_test

// Unit tests for validateMilestoneInput edge cases (finding #3 from rereview audit).
// Exercises boundary conditions that are pure logic — no DB needed.
// The validation runs at the top of AddMilestone before any store call, so a
// minimal fake MultipartyContractStore is sufficient.
//
// Cases covered (table-driven):
//   - zero amount → ErrValidation
//   - negative amount → ErrValidation
//   - over-max amount (>999999999999.99) → ErrValidation
//   - >2 decimal places → ErrValidation
//   - valid 2-decimal amount → no error from validation
//   - valid whole-number amount → no error from validation

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMultipartyContractStore is a minimal in-memory implementation of
// store.MultipartyContractStore used in validation unit tests.
// Only GetByID is needed by AddMilestone before the validation short-circuits.
type fakeMultipartyContractStore struct {
	contracts map[uuid.UUID]*domain.MultipartyContract
}

func newFakeMultipartyContractStore(c *domain.MultipartyContract) *fakeMultipartyContractStore {
	s := &fakeMultipartyContractStore{contracts: make(map[uuid.UUID]*domain.MultipartyContract)}
	s.contracts[c.ID] = c

	return s
}

func (f *fakeMultipartyContractStore) Create(_ context.Context, c *domain.MultipartyContract) error {
	f.contracts[c.ID] = c

	return nil
}

func (f *fakeMultipartyContractStore) GetByID(_ context.Context, id uuid.UUID) (*domain.MultipartyContract, error) {
	c, ok := f.contracts[id]
	if !ok {
		return nil, domain.ErrMultipartyContractNotFound
	}

	return c, nil
}

func (f *fakeMultipartyContractStore) GetByTenderID(_ context.Context, tenderID uuid.UUID) (*domain.MultipartyContract, error) {
	for _, c := range f.contracts {
		if c.TenderID == tenderID {
			return c, nil
		}
	}

	return nil, domain.ErrMultipartyContractNotFound
}

// GetByIDForUpdate delegates to GetByID in the fake; it does NOT issue a SELECT FOR
// UPDATE — the real Postgres implementation (postgres.MultipartyContractStore) does.
// This is intentional for unit tests: no DB transaction exists, and the test only
// exercises code paths that do not depend on the serialization the lock provides.
func (f *fakeMultipartyContractStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.MultipartyContract, error) {
	return f.GetByID(ctx, id)
}

func (f *fakeMultipartyContractStore) Update(_ context.Context, c *domain.MultipartyContract) error {
	f.contracts[c.ID] = c

	return nil
}

// fakeMilestoneStore implements store.MilestoneStore for validation unit tests.
// No operation should be reached because validation short-circuits before any DB write.
type fakeMilestoneStore struct{}

func (fakeMilestoneStore) Create(_ context.Context, _ *domain.Milestone) error {
	panic("fakeMilestoneStore.Create should not be called in validation-only tests")
}

func (fakeMilestoneStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.Milestone, error) {
	panic("fakeMilestoneStore.GetByID should not be called in validation-only tests")
}

func (fakeMilestoneStore) ListByContract(_ context.Context, _ uuid.UUID) ([]*domain.Milestone, error) {
	panic("fakeMilestoneStore.ListByContract should not be called in validation-only tests")
}

func (fakeMilestoneStore) MarkCompleted(_ context.Context, _ uuid.UUID, _ time.Time) (*domain.Milestone, error) {
	panic("fakeMilestoneStore.MarkCompleted should not be called in validation-only tests")
}

func (fakeMilestoneStore) SumAmountsByContract(_ context.Context, _ uuid.UUID) (decimal.Decimal, error) {
	panic("fakeMilestoneStore.SumAmountsByContract should not be called in validation-only tests")
}

// fakeMilestonePartyStore is a no-op MultipartyPartyStore for validation unit tests.
type fakeMilestonePartyStore struct{}

func (fakeMilestonePartyStore) AddParty(_ context.Context, _ *domain.MultipartyContractParty) error {
	panic("fakeMilestonePartyStore.AddParty should not be called in validation-only tests")
}

func (fakeMilestonePartyStore) GetActivePartyByVendor(_ context.Context, _, _ uuid.UUID) (*domain.MultipartyContractParty, error) {
	panic("fakeMilestonePartyStore.GetActivePartyByVendor should not be called in validation-only tests")
}

func (fakeMilestonePartyStore) GetActivePartyByID(_ context.Context, _ uuid.UUID) (*domain.MultipartyContractParty, error) {
	panic("fakeMilestonePartyStore.GetActivePartyByID should not be called in validation-only tests")
}

func (fakeMilestonePartyStore) UpdatePartyShare(_ context.Context, _, _ uuid.UUID, _ int) (*domain.MultipartyContractParty, error) {
	panic("fakeMilestonePartyStore.UpdatePartyShare should not be called in validation-only tests")
}

func (fakeMilestonePartyStore) ListActiveByContract(_ context.Context, _ uuid.UUID) ([]*domain.MultipartyContractParty, error) {
	panic("fakeMilestonePartyStore.ListActiveByContract should not be called in validation-only tests")
}

func (fakeMilestonePartyStore) SumActiveBps(_ context.Context, _ uuid.UUID) (int, error) {
	panic("fakeMilestonePartyStore.SumActiveBps should not be called in validation-only tests")
}

func (fakeMilestonePartyStore) CountActiveParties(_ context.Context, _ uuid.UUID) (int, error) {
	panic("fakeMilestonePartyStore.CountActiveParties should not be called in validation-only tests")
}

// fakeMilestoneTxManager implements store.MilestoneTxManager.
// Validation short-circuits before any tx is opened so this must never be called.
type fakeMilestoneTxManager struct{}

func (fakeMilestoneTxManager) WithMilestoneTx(
	_ context.Context,
	_ func(context.Context, store.MultipartyContractStore, store.MilestoneStore, store.OutboxStore) error,
) error {
	panic("fakeMilestoneTxManager.WithMilestoneTx should not be called in validation-only tests")
}

// buildValidationMilestoneSvc returns a MilestoneService backed by fake stores, plus
// an ACTIVE contract owned by posterID that callers can use in AddMilestone calls.
func buildValidationMilestoneSvc() (*service.MilestoneService, *domain.MultipartyContract, uuid.UUID) {
	posterID := uuid.New()
	now := time.Now().UTC()

	contract := &domain.MultipartyContract{
		ID:           uuid.New(),
		TenderID:     uuid.New(),
		Status:       domain.MultipartyContractStatusActive,
		Version:      1,
		PosterUserID: &posterID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	svc := service.NewMilestoneService(
		newFakeMultipartyContractStore(contract),
		fakeMilestoneStore{},
		fakeMilestonePartyStore{},
		fakeMilestoneTxManager{},
		events.NewNoopPublisher(),
	)

	return svc, contract, posterID
}

// TestValidateMilestoneInput_ErrorCases covers finding #3 from the rereview audit:
// upper-bound (numeric(14,2) overflow), >2 decimal places, zero and negative amounts.
// The panicking fakeMilestoneStore ensures validation short-circuits before any DB write.
func TestValidateMilestoneInput_ErrorCases(t *testing.T) {
	cases := []struct {
		name   string
		amount decimal.Decimal
	}{
		{
			name:   "zero amount → ErrValidation",
			amount: decimal.Zero,
		},
		{
			name:   "negative amount → ErrValidation",
			amount: decimal.NewFromFloat(-0.01),
		},
		{
			name:   "over max numeric(14,2) → ErrValidation",
			amount: decimal.NewFromFloat(1_000_000_000_000.00), // 1e12, exceeds 999999999999.99
		},
		{
			name:   "three decimal places → ErrValidation",
			amount: decimal.NewFromFloat(100.001),
		},
		{
			name:   "four decimal places → ErrValidation",
			amount: decimal.RequireFromString("1.0001"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// buildValidationMilestoneSvc uses panicking store — validation MUST short-circuit before reaching Create.
			svc, contract, posterID := buildValidationMilestoneSvc()

			_, err := svc.AddMilestone(context.Background(), &service.AddMilestoneInput{
				ContractID: contract.ID,
				CallerID:   posterID,
				Name:       "Test Milestone",
				Amount:     tc.amount,
				Currency:   "TWD",
				Sequence:   1,
			})

			require.Error(t, err, "amount %s should produce a validation error", tc.amount)
			assert.ErrorIs(t, err, domain.ErrValidation,
				"amount %s should produce ErrValidation, got: %v", tc.amount, err)
		})
	}
}

// TestValidateMilestoneInput_HappyCases proves that valid amounts pass validation.
// Uses a non-panicking store so the full AddMilestone path completes.
func TestValidateMilestoneInput_HappyCases(t *testing.T) {
	cases := []struct {
		name   string
		amount decimal.Decimal
	}{
		{
			name:   "valid whole number → ok",
			amount: decimal.NewFromInt(1000),
		},
		{
			name:   "valid 1-decimal → ok",
			amount: decimal.NewFromFloat(100.5),
		},
		{
			name:   "valid 2-decimal → ok",
			amount: decimal.NewFromFloat(999.99),
		},
		{
			name:   "max allowed (999999999999.99) → ok",
			amount: decimal.RequireFromString("999999999999.99"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, contract, posterID := buildNonPanickingMilestoneSvc()

			_, err := svc.AddMilestone(context.Background(), &service.AddMilestoneInput{
				ContractID: contract.ID,
				CallerID:   posterID,
				Name:       "Test Milestone",
				Amount:     tc.amount,
				Currency:   "TWD",
				Sequence:   1,
			})
			require.NoError(t, err, "amount %s should not return an error", tc.amount)
		})
	}
}

// noopMilestoneStore implements store.MilestoneStore with an in-memory Create
// so happy-path validation tests do not panic.
type noopMilestoneStore struct {
	milestones []*domain.Milestone
}

func (s *noopMilestoneStore) Create(_ context.Context, m *domain.Milestone) error {
	s.milestones = append(s.milestones, m)

	return nil
}

func (s *noopMilestoneStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Milestone, error) {
	for _, m := range s.milestones {
		if m.ID == id {
			return m, nil
		}
	}

	return nil, domain.ErrMilestoneNotFound
}

func (s *noopMilestoneStore) ListByContract(_ context.Context, contractID uuid.UUID) ([]*domain.Milestone, error) {
	var out []*domain.Milestone

	for _, m := range s.milestones {
		if m.MultiContractID == contractID {
			out = append(out, m)
		}
	}

	return out, nil
}

func (s *noopMilestoneStore) MarkCompleted(_ context.Context, id uuid.UUID, completedAt time.Time) (*domain.Milestone, error) {
	for _, m := range s.milestones {
		if m.ID == id {
			m.Status = domain.MilestoneStatusCompleted
			m.CompletedAt = &completedAt

			return m, nil
		}
	}

	return nil, domain.ErrMilestoneNotFound
}

func (s *noopMilestoneStore) SumAmountsByContract(_ context.Context, contractID uuid.UUID) (decimal.Decimal, error) {
	sum := decimal.Zero
	for _, m := range s.milestones {
		if m.MultiContractID == contractID {
			sum = sum.Add(m.Amount)
		}
	}

	return sum, nil
}

// buildNonPanickingMilestoneSvc returns a MilestoneService whose MilestoneStore.Create
// succeeds in-memory. Used for happy-path validation tests that must not panic.
func buildNonPanickingMilestoneSvc() (*service.MilestoneService, *domain.MultipartyContract, uuid.UUID) {
	posterID := uuid.New()
	now := time.Now().UTC()

	contract := &domain.MultipartyContract{
		ID:           uuid.New(),
		TenderID:     uuid.New(),
		Status:       domain.MultipartyContractStatusActive,
		Version:      1,
		PosterUserID: &posterID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	svc := service.NewMilestoneService(
		newFakeMultipartyContractStore(contract),
		&noopMilestoneStore{},
		fakeMilestonePartyStore{},
		fakeMilestoneTxManager{},
		events.NewNoopPublisher(),
	)

	return svc, contract, posterID
}
