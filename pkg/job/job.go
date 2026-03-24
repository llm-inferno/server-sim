package job

import (
	"sync"

	"github.com/google/uuid"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// Status represents the current state of a simulation job.
type Status string

const (
	StatusPending   Status = "pending"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Job holds the state of a single simulation job.
type Job struct {
	ID     string
	Status Status
	Result *evaluator.AnalysisData
	Error  string
}

// Manager stores and manages simulation jobs in memory.
type Manager struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// NewManager creates a new job Manager.
func NewManager() *Manager {
	return &Manager{jobs: make(map[string]*Job)}
}

// Create registers a new pending job and returns its ID.
func (m *Manager) Create() string {
	id := uuid.New().String()
	m.mu.Lock()
	m.jobs[id] = &Job{ID: id, Status: StatusPending}
	m.mu.Unlock()
	return id
}

// Complete marks a job as completed with the given result.
func (m *Manager) Complete(id string, result evaluator.AnalysisData) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.Status = StatusCompleted
		j.Result = &result
	}
	m.mu.Unlock()
}

// Fail marks a job as failed with the given error message.
func (m *Manager) Fail(id string, errMsg string) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.Status = StatusFailed
		j.Error = errMsg
	}
	m.mu.Unlock()
}

// Get retrieves a job by ID. Returns nil if not found.
func (m *Manager) Get(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}
