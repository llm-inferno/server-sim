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
	ID             string
	Status         Status
	EffectiveInput evaluator.ProblemData // load/allocation actually run (post saturation retry)
	Result         *evaluator.AnalysisData
	Error          string
	CompletedAt    time.Time // zero while pending
	seq            uint64    // monotonic completion order; 0 until completed
}

// Manager stores and manages simulation jobs in memory.
type Manager struct {
	mu        sync.RWMutex
	jobs      map[string]*Job
	ttl       time.Duration
	seq       uint64 // monotonically increasing; stamped on each completion
	done      chan struct{}
	closeOnce sync.Once
}

// NewManager creates a new job Manager. Completed and failed jobs are evicted
// after ttl. A sweep runs every ttl/2 (minimum 30s). Call Close to stop the
// background sweep goroutine.
func NewManager(ttl time.Duration) *Manager {
	m := &Manager{jobs: make(map[string]*Job), ttl: ttl, done: make(chan struct{})}
	go m.sweepLoop()
	return m
}

// Close stops the background sweep goroutine. It is idempotent and safe to call
// from multiple goroutines.
func (m *Manager) Close() {
	m.closeOnce.Do(func() { close(m.done) })
}

func (m *Manager) sweepLoop() {
	interval := m.ttl / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

func (m *Manager) sweep() {
	cutoff := time.Now().Add(-m.ttl)
	m.mu.Lock()
	for id, j := range m.jobs {
		if !j.CompletedAt.IsZero() && j.CompletedAt.Before(cutoff) {
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

// Complete marks a job as completed with the effective input that produced the result.
func (m *Manager) Complete(id string, effectiveInput evaluator.ProblemData, result evaluator.AnalysisData) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		m.seq++
		j.Status = StatusCompleted
		j.EffectiveInput = effectiveInput
		j.Result = &result
		j.CompletedAt = time.Now()
		j.seq = m.seq
	}
	m.mu.Unlock()
}

// Latest returns a snapshot copy of the most-recently-completed job, or nil if
// none has completed. The returned pointer is a fresh allocation; callers may
// read its fields without holding any lock.
func (m *Manager) Latest() *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var latest *Job
	for _, j := range m.jobs {
		if j.Status != StatusCompleted {
			continue
		}
		if latest == nil || j.seq > latest.seq {
			latest = j
		}
	}
	if latest == nil {
		return nil
	}
	cp := *latest
	return &cp
}

// Fail marks a job as failed with the given error message.
func (m *Manager) Fail(id string, errMsg string) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.Status = StatusFailed
		j.Error = errMsg
		j.CompletedAt = time.Now()
	}
	m.mu.Unlock()
}

// Get retrieves a snapshot copy of a job by ID. Returns nil if not found.
// The returned pointer is a fresh allocation; callers may read its fields
// without holding any lock.
func (m *Manager) Get(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil
	}
	cp := *j
	return &cp
}
