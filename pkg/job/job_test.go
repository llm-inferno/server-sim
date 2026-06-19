package job

import (
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
