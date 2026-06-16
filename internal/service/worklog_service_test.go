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

// --- fake worklog store ---

type fakeWorklogStore struct {
	worklogs map[uuid.UUID]*domain.Worklog
}

func newFakeWorklogStore() *fakeWorklogStore {
	return &fakeWorklogStore{worklogs: make(map[uuid.UUID]*domain.Worklog)}
}

func (f *fakeWorklogStore) Create(_ context.Context, w *domain.Worklog) error {
	f.worklogs[w.ID] = w

	return nil
}

func (f *fakeWorklogStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Worklog, error) {
	w, ok := f.worklogs[id]
	if !ok {
		return nil, domain.ErrWorklogNotFound
	}

	return w, nil
}

func (f *fakeWorklogStore) ListByContract(_ context.Context, contractID uuid.UUID) ([]*domain.Worklog, error) {
	var result []*domain.Worklog

	for _, w := range f.worklogs {
		if w.ContractID == contractID {
			result = append(result, w)
		}
	}

	return result, nil
}

func (f *fakeWorklogStore) SoftDelete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.worklogs[id]; !ok {
		return domain.ErrWorklogNotFound
	}

	delete(f.worklogs, id)

	return nil
}

// --- tests ---

func TestCreateWorklog(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	tests := []struct {
		name     string
		callerID uuid.UUID
		minutes  int
		desc     string
		wantErr  bool
		errIs    error
	}{
		{
			name:     "happy path: freelancer logs 60 min",
			callerID: freelancerID,
			minutes:  60,
			desc:     "Implemented feature X",
			wantErr:  false,
		},
		{
			name:     "minutes 0 rejected",
			callerID: clientID,
			minutes:  0,
			desc:     testValDesc,
			wantErr:  true,
			errIs:    domain.ErrValidation,
		},
		{
			name:     "minutes > 1440 rejected",
			callerID: clientID,
			minutes:  1441,
			desc:     testValDesc,
			wantErr:  true,
			errIs:    domain.ErrValidation,
		},
		{
			name:     "non-party cannot log time",
			callerID: uuid.New(),
			minutes:  60,
			desc:     testValDesc,
			wantErr:  true,
			errIs:    domain.ErrNotFound,
		},
		{
			name:     "exactly 1440 minutes accepted",
			callerID: clientID,
			minutes:  1440,
			desc:     "full day",
			wantErr:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeContractStore()
			ws := newFakeWorklogStore()
			svc := service.NewWorklogService(cs, ws)

			c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
			require.NoError(t, cs.Create(context.Background(), c))

			result, err := svc.CreateWorklog(context.Background(), service.CreateWorklogInput{
				ContractID:  c.ID,
				UserID:      tc.callerID,
				Description: tc.desc,
				Minutes:     tc.minutes,
			})

			if tc.wantErr {
				require.Error(t, err)

				if tc.errIs != nil {
					require.ErrorIs(t, err, tc.errIs)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.minutes, result.Minutes)
			assert.Equal(t, c.ID, result.ContractID)
		})
	}
}

func TestDeleteWorklog_AuthorOnly(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	t.Run("author can delete own worklog", func(t *testing.T) {
		cs := newFakeContractStore()
		ws := newFakeWorklogStore()
		svc := service.NewWorklogService(cs, ws)

		c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c))

		wl, err := svc.CreateWorklog(context.Background(), service.CreateWorklogInput{
			ContractID:  c.ID,
			UserID:      freelancerID,
			Description: "work done",
			Minutes:     120,
		})
		require.NoError(t, err)

		err = svc.DeleteWorklog(context.Background(), c.ID, wl.ID, freelancerID)
		require.NoError(t, err)
	})

	t.Run("other party cannot delete worklog (author-only)", func(t *testing.T) {
		cs := newFakeContractStore()
		ws := newFakeWorklogStore()
		svc := service.NewWorklogService(cs, ws)

		c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c))

		wl, err := svc.CreateWorklog(context.Background(), service.CreateWorklogInput{
			ContractID:  c.ID,
			UserID:      freelancerID,
			Description: "work done",
			Minutes:     60,
		})
		require.NoError(t, err)

		// Client tries to delete freelancer's worklog.
		err = svc.DeleteWorklog(context.Background(), c.ID, wl.ID, clientID)
		require.ErrorIs(t, err, domain.ErrWorklogNotFound)
	})

	t.Run("cross-contract worklog smuggling returns 404", func(t *testing.T) {
		cs := newFakeContractStore()
		ws := newFakeWorklogStore()
		svc := service.NewWorklogService(cs, ws)

		c1 := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		c2 := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c1))
		require.NoError(t, cs.Create(context.Background(), c2))

		wl, err := svc.CreateWorklog(context.Background(), service.CreateWorklogInput{
			ContractID:  c1.ID,
			UserID:      clientID,
			Description: "work",
			Minutes:     30,
		})
		require.NoError(t, err)

		// Try to delete wl via c2 path (cross-contract smuggling).
		err = svc.DeleteWorklog(context.Background(), c2.ID, wl.ID, clientID)
		require.ErrorIs(t, err, domain.ErrWorklogNotFound)
	})
}
