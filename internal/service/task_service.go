package service

import (
	"context"
	"fmt"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
)

// TaskService handles task business logic.
type TaskService struct {
	contracts store.ContractStore
	tasks     store.TaskStore
}

// NewTaskService returns a TaskService.
func NewTaskService(contracts store.ContractStore, tasks store.TaskStore) *TaskService {
	return &TaskService{contracts: contracts, tasks: tasks}
}

// CreateTaskInput carries validated input for creating a task.
type CreateTaskInput struct {
	ContractID     uuid.UUID
	CallerID       uuid.UUID
	Title          string
	AssigneeUserID *uuid.UUID
}

// CreateTask creates a task under a contract. Caller must be a party.
// Assignee must be nil or a party to the contract.
func (s *TaskService) CreateTask(ctx context.Context, in CreateTaskInput) (*domain.Task, error) {
	c, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, in.CallerID); err != nil {
		return nil, err
	}

	if err := validateTitle(in.Title); err != nil {
		return nil, err
	}

	// If assignee is set, it must be one of the two parties.
	if in.AssigneeUserID != nil {
		if *in.AssigneeUserID != c.ClientUserID && *in.AssigneeUserID != c.FreelancerUserID {
			return nil, fmt.Errorf("%w: assignee must be a party to this contract", domain.ErrValidation)
		}
	}

	now := time.Now().UTC()
	t := &domain.Task{
		ID:             uuid.New(),
		ContractID:     in.ContractID,
		Title:          in.Title,
		Status:         domain.TaskStatusTodo,
		AssigneeUserID: in.AssigneeUserID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := s.tasks.Create(ctx, t); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	return t, nil
}

// ListTasks returns all tasks for a contract. Caller must be a party.
func (s *TaskService) ListTasks(ctx context.Context, contractID, callerID uuid.UUID) ([]*domain.Task, error) {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, callerID); err != nil {
		return nil, err
	}

	tasks, err := s.tasks.ListByContract(ctx, contractID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	return tasks, nil
}

// UpdateTaskInput carries validated input for updating a task.
type UpdateTaskInput struct {
	ContractID     uuid.UUID
	TaskID         uuid.UUID
	CallerID       uuid.UUID
	Title          *string
	Status         *domain.TaskStatus
	AssigneeUserID *uuid.UUID
	ClearAssignee  bool // true means set assignee to nil
}

// UpdateTask applies a partial update to a task.
func (s *TaskService) UpdateTask(ctx context.Context, in *UpdateTaskInput) (*domain.Task, error) {
	c, err := s.contracts.GetByID(ctx, in.ContractID)
	if err != nil {
		return nil, err
	}

	if err := assertParty(c, in.CallerID); err != nil {
		return nil, err
	}

	t, err := s.tasks.GetByID(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}

	// Cross-contract child ID smuggling prevention: task must belong to this contract.
	if t.ContractID != in.ContractID {
		return nil, domain.ErrTaskNotFound
	}

	if in.Title != nil {
		if err := validateTitle(*in.Title); err != nil {
			return nil, err
		}

		t.Title = *in.Title
	}

	if in.Status != nil {
		switch *in.Status {
		case domain.TaskStatusTodo, domain.TaskStatusDoing, domain.TaskStatusDone:
			t.Status = *in.Status
		default:
			return nil, fmt.Errorf("%w: invalid task status", domain.ErrValidation)
		}
	}

	if in.ClearAssignee {
		t.AssigneeUserID = nil
	} else if in.AssigneeUserID != nil {
		if *in.AssigneeUserID != c.ClientUserID && *in.AssigneeUserID != c.FreelancerUserID {
			return nil, fmt.Errorf("%w: assignee must be a party to this contract", domain.ErrValidation)
		}

		t.AssigneeUserID = in.AssigneeUserID
	}

	if err := s.tasks.Update(ctx, t); err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}

	return t, nil
}

// DeleteTask soft-deletes a task. Caller must be a party to the contract.
func (s *TaskService) DeleteTask(ctx context.Context, contractID, taskID, callerID uuid.UUID) error {
	c, err := s.contracts.GetByID(ctx, contractID)
	if err != nil {
		return err
	}

	if err := assertParty(c, callerID); err != nil {
		return err
	}

	t, err := s.tasks.GetByID(ctx, taskID)
	if err != nil {
		return err
	}

	// Cross-contract child ID smuggling prevention.
	if t.ContractID != contractID {
		return domain.ErrTaskNotFound
	}

	return s.tasks.SoftDelete(ctx, taskID)
}
