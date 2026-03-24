package job

import (
	"sync"
	"time"

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
	ID          string
	Status      Status
	Result      *evaluator.AnalysisData
	Error       string
	completedAt time.Time // zero while pending
}

// Manager stores and manages simulation jobs in memory.
type Manager struct {
	mu   sync.RWMutex
	jobs map[string]*Job
	ttl  time.Duration
}

// NewManager creates a new job Manager. Completed and failed jobs are evicted
// after ttl. A sweep runs every ttl/2 (minimum 30s).
func NewManager(ttl time.Duration) *Manager {
	m := &Manager{jobs: make(map[string]*Job), ttl: ttl}
	go m.sweepLoop()
	return m
}

func (m *Manager) sweepLoop() {
	interval := m.ttl / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		m.sweep()
	}
}

func (m *Manager) sweep() {
	cutoff := time.Now().Add(-m.ttl)
	m.mu.Lock()
	for id, j := range m.jobs {
		if !j.completedAt.IsZero() && j.completedAt.Before(cutoff) {
			delete(m.jobs, id)
		}
	}
	m.mu.Unlock()
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
		j.completedAt = time.Now()
	}
	m.mu.Unlock()
}

// Fail marks a job as failed with the given error message.
func (m *Manager) Fail(id string, errMsg string) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.Status = StatusFailed
		j.Error = errMsg
		j.completedAt = time.Now()
	}
	m.mu.Unlock()
}

// Get retrieves a job by ID. Returns nil if not found.
func (m *Manager) Get(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}
