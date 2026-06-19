package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
	"github.com/llm-inferno/server-sim/pkg/job"
)

type okSolver struct{ ad evaluator.AnalysisData }

func (s okSolver) SolveCtx(_ context.Context, _ evaluator.ProblemData) (evaluator.AnalysisData, error) {
	return s.ad, nil
}

func writeLabels(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "labels")
	if err := os.WriteFile(p, []byte(sampleLabels), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunOncePublishesLatest(t *testing.T) {
	dir := t.TempDir()
	writeLabels(t, dir)
	cfg := config.Config{SaturationPolicy: config.SaturationPolicyPassThrough, LabelsDir: dir, TickInterval: time.Second}
	jobs := job.NewManager(60 * time.Second)
	l := NewLoop(cfg, jobs, okSolver{ad: evaluator.AnalysisData{AvgITL: 9, Throughput: 5}})

	l.runOnce(context.Background())

	latest := jobs.Latest()
	if latest == nil {
		t.Fatal("no latest job after runOnce")
	}
	if latest.Result.AvgITL != 9 {
		t.Fatalf("latest result wrong: %+v", latest.Result)
	}
	if latest.EffectiveInput.MaxConcurrency != 32 {
		t.Fatalf("effective input concurrency = %d, want 32", latest.EffectiveInput.MaxConcurrency)
	}
}

func TestRunOnceSkipsWhenLabelsMissing(t *testing.T) {
	dir := t.TempDir() // no labels file
	cfg := config.Config{SaturationPolicy: config.SaturationPolicyPassThrough, LabelsDir: dir}
	jobs := job.NewManager(60 * time.Second)
	NewLoop(cfg, jobs, okSolver{}).runOnce(context.Background())
	if jobs.Latest() != nil {
		t.Fatal("should not publish when labels missing")
	}
}
