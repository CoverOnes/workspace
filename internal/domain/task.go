package domain

import (
	"time"

	"github.com/google/uuid"
)

// TaskStatus represents the state of a task.
type TaskStatus string

const (
	TaskStatusTodo  TaskStatus = "TODO"
	TaskStatusDoing TaskStatus = "DOING"
	TaskStatusDone  TaskStatus = "DONE"
)

// Task is a work item hanging off a contract via soft ref.
// CRUD by either contract party. Assignee must be one of the two parties.
type Task struct {
	ID             uuid.UUID  `json:"id"`
	ContractID     uuid.UUID  `json:"contractId"`
	Title          string     `json:"title"`
	Status         TaskStatus `json:"status"`
	AssigneeUserID *uuid.UUID `json:"assigneeUserId,omitempty"`
	DeletedAt      *time.Time `json:"deletedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}
