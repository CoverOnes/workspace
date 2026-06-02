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

// --- fake task store ---

type fakeTaskStore struct {
	tasks map[uuid.UUID]*domain.Task
}

func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{tasks: make(map[uuid.UUID]*domain.Task)}
}

func (f *fakeTaskStore) Create(_ context.Context, t *domain.Task) error {
	f.tasks[t.ID] = t

	return nil
}

func (f *fakeTaskStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Task, error) {
	t, ok := f.tasks[id]
	if !ok {
		return nil, domain.ErrTaskNotFound
	}

	return t, nil
}

func (f *fakeTaskStore) ListByContract(_ context.Context, contractID uuid.UUID) ([]*domain.Task, error) {
	var result []*domain.Task

	for _, t := range f.tasks {
		if t.ContractID == contractID {
			result = append(result, t)
		}
	}

	return result, nil
}

func (f *fakeTaskStore) Update(_ context.Context, t *domain.Task) error {
	if _, ok := f.tasks[t.ID]; !ok {
		return domain.ErrTaskNotFound
	}

	f.tasks[t.ID] = t

	return nil
}

func (f *fakeTaskStore) SoftDelete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.tasks[id]; !ok {
		return domain.ErrTaskNotFound
	}

	delete(f.tasks, id)

	return nil
}

// --- tests ---

func TestCreateTask(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()

	tests := []struct {
		name     string
		callerID uuid.UUID
		title    string
		assignee *uuid.UUID
		wantErr  bool
		errIs    error
	}{
		{
			name:     "client creates task (no assignee)",
			callerID: clientID,
			title:    "Implement feature X",
			wantErr:  false,
		},
		{
			name:     "freelancer creates task with assignee = client",
			callerID: freelancerID,
			title:    "Review PR",
			assignee: &clientID,
			wantErr:  false,
		},
		{
			name:     "non-party cannot create task",
			callerID: thirdPartyID,
			title:    "Valid title",
			wantErr:  true,
			errIs:    domain.ErrNotFound,
		},
		{
			name:     "assignee not a party is rejected",
			callerID: clientID,
			title:    "Task",
			assignee: &thirdPartyID,
			wantErr:  true,
			errIs:    domain.ErrValidation,
		},
		{
			name:     "empty title rejected",
			callerID: clientID,
			title:    "",
			wantErr:  true,
			errIs:    domain.ErrValidation,
		},
	}

	//nolint:dupl // table-driven test body; similar structure to worklog tests is intentional, not a copy-paste error
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeContractStore()
			ts := newFakeTaskStore()
			svc := service.NewTaskService(cs, ts)

			c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
			require.NoError(t, cs.Create(context.Background(), c))

			result, err := svc.CreateTask(context.Background(), service.CreateTaskInput{
				ContractID:     c.ID,
				CallerID:       tc.callerID,
				Title:          tc.title,
				AssigneeUserID: tc.assignee,
			})

			if tc.wantErr {
				require.Error(t, err)

				if tc.errIs != nil {
					require.ErrorIs(t, err, tc.errIs)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, domain.TaskStatusTodo, result.Status)
			assert.Equal(t, c.ID, result.ContractID)
		})
	}
}

func TestDeleteTask(t *testing.T) {
	clientID := uuid.New()
	freelancerID := uuid.New()

	t.Run("party can soft-delete task", func(t *testing.T) {
		cs := newFakeContractStore()
		ts := newFakeTaskStore()
		svc := service.NewTaskService(cs, ts)

		c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c))

		task, err := svc.CreateTask(context.Background(), service.CreateTaskInput{
			ContractID: c.ID,
			CallerID:   clientID,
			Title:      "Delete me",
		})
		require.NoError(t, err)

		err = svc.DeleteTask(context.Background(), c.ID, task.ID, clientID)
		require.NoError(t, err)
	})

	t.Run("cross-contract task smuggling returns 404", func(t *testing.T) {
		cs := newFakeContractStore()
		ts := newFakeTaskStore()
		svc := service.NewTaskService(cs, ts)

		c1 := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		c2 := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c1))
		require.NoError(t, cs.Create(context.Background(), c2))

		// Create task under c1.
		task, err := svc.CreateTask(context.Background(), service.CreateTaskInput{
			ContractID: c1.ID,
			CallerID:   clientID,
			Title:      "Task on c1",
		})
		require.NoError(t, err)

		// Try to delete it via c2 path (smuggling).
		err = svc.DeleteTask(context.Background(), c2.ID, task.ID, clientID)
		require.ErrorIs(t, err, domain.ErrTaskNotFound)
	})

	t.Run("non-party cannot delete task", func(t *testing.T) {
		cs := newFakeContractStore()
		ts := newFakeTaskStore()
		svc := service.NewTaskService(cs, ts)

		c := makeContract(clientID, freelancerID, domain.ContractStatusActive)
		require.NoError(t, cs.Create(context.Background(), c))

		task, err := svc.CreateTask(context.Background(), service.CreateTaskInput{
			ContractID: c.ID,
			CallerID:   clientID,
			Title:      "task",
		})
		require.NoError(t, err)

		err = svc.DeleteTask(context.Background(), c.ID, task.ID, uuid.New())
		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}
