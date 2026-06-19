package job

import (
	"sync"
	"testing"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func TestCompleteStoresEffectiveInputAndLatest(t *testing.T) {
	m := NewManager(60 * 1e9) // 60s ttl
	id := m.Create()
	in := evaluator.ProblemData{RPS: 5, MaxConcurrency: 32, Model: "m", Accelerator: "H100"}
	out := evaluator.AnalysisData{AvgITL: 10, AvgTTFT: 100, Throughput: 5}
	m.Complete(id, in, out)

	j := m.Get(id)
	if j.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", j.Status)
	}
	if j.EffectiveInput.MaxConcurrency != 32 || j.Result.AvgITL != 10 {
		t.Fatalf("envelope not stored: %+v", j)
	}
	if j.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt not set")
	}

	latest := m.Latest()
	if latest == nil || latest.ID != id {
		t.Fatalf("Latest() = %v, want job %s", latest, id)
	}
}

func TestLatestReturnsNilWhenNoneCompleted(t *testing.T) {
	m := NewManager(60 * 1e9)
	m.Create() // pending only
	if m.Latest() != nil {
		t.Fatalf("Latest() should be nil when no job has completed")
	}
}

func TestLatestPicksMostRecentCompletion(t *testing.T) {
	m := NewManager(60 * 1e9)
	id1 := m.Create()
	id2 := m.Create()
	m.Complete(id1, evaluator.ProblemData{RPS: 1}, evaluator.AnalysisData{})
	m.Complete(id2, evaluator.ProblemData{RPS: 2}, evaluator.AnalysisData{})
	if m.Latest().ID != id2 {
		t.Fatalf("Latest().ID = %s, want %s (most recent)", m.Latest().ID, id2)
	}
}

// TestLatestUsesCompletionOrderNotTimestamp verifies that Latest() picks the
// job completed last by monotonic sequence, so ties on the wall-clock
// CompletedAt (same tick) and randomized map iteration order can't make it
// non-deterministic. id2 completes after id1, so it must win even when both
// share a CompletedAt timestamp.
func TestLatestUsesCompletionOrderNotTimestamp(t *testing.T) {
	m := NewManager(60 * 1e9)
	id1 := m.Create()
	id2 := m.Create()
	m.Complete(id1, evaluator.ProblemData{RPS: 1}, evaluator.AnalysisData{})
	m.Complete(id2, evaluator.ProblemData{RPS: 2}, evaluator.AnalysisData{})
	// Force identical wall-clock timestamps to isolate the sequence tie-break.
	m.mu.Lock()
	ts := m.jobs[id1].CompletedAt
	m.jobs[id2].CompletedAt = ts
	m.mu.Unlock()
	for i := 0; i < 50; i++ { // repeat: map iteration order is randomized
		if got := m.Latest(); got == nil || got.ID != id2 {
			t.Fatalf("Latest().ID = %v, want %s (completed last)", got, id2)
		}
	}
}

// TestGetLatestRace is a focused regression test for the data race between
// Complete (writer) and Get/Latest (readers). The race detector flags
// unsynchronized field reads on the *Job pointer returned by Get/Latest when
// a concurrent Complete mutates the same fields under its write lock.
//
// Root cause: Get/Latest return the live *Job pointer after releasing the lock.
// A concurrent Complete then writes j.Status/j.Result/j.CompletedAt under
// its own Lock, producing a write/read data race on the same *Job memory.
//
// The test below injects a shared *Job directly into the Manager's map to
// ensure that the writer (Complete via direct field mutation) and the
// readers (Get/Latest field reads) always operate on the same pointer.
//
// Run with: go test -race -run TestGetLatestRace ./pkg/job/
func TestGetLatestRace(t *testing.T) {
	const iterations = 500
	m := NewManager(60 * 1e9)

	// Inject a shared *Job directly. Both the writer goroutine (which calls
	// Complete, i.e. m.mu.Lock + j.Status=... + j.Result=...) and the reader
	// goroutines (Get/Latest, which return this same pointer and then read
	// fields outside the lock) will race on exactly this object.
	sharedJob := &Job{ID: "shared", Status: StatusPending}
	m.mu.Lock()
	m.jobs[sharedJob.ID] = sharedJob
	m.mu.Unlock()

	var wg sync.WaitGroup

	// Writer: repeatedly Complete the same job so it keeps writing
	// j.Status / j.Result / j.CompletedAt on the shared *Job under m.mu.Lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			m.Complete(sharedJob.ID,
				evaluator.ProblemData{RPS: float32(i)},
				evaluator.AnalysisData{AvgITL: float32(i)})
			// Reset to pending so the cycle can repeat.
			m.mu.Lock()
			if j, ok := m.jobs[sharedJob.ID]; ok {
				j.Status = StatusPending
			}
			m.mu.Unlock()
		}
	}()

	// Reader A: Get returns the live *Job pointer; reading its fields outside
	// the lock races with the writer above.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			j := m.Get(sharedJob.ID)
			if j != nil {
				_ = j.Status // DATA RACE: writer sets j.Status under Lock
				_ = j.Result // DATA RACE: writer sets j.Result under Lock
				_ = j.CompletedAt
			}
		}
	}()

	// Reader B: Latest also returns the live *Job pointer when it finds one.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			j := m.Latest()
			if j != nil {
				_ = j.Status // DATA RACE
				_ = j.EffectiveInput
				_ = j.Result // DATA RACE
				_ = j.CompletedAt
			}
		}
	}()

	wg.Wait()
}
