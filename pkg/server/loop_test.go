package server

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
	"github.com/llm-inferno/server-sim/pkg/job"
)

type cancelSolver struct{}

func (cancelSolver) SolveCtx(context.Context, evaluator.ProblemData) (evaluator.AnalysisData, error) {
	return evaluator.AnalysisData{}, context.Canceled
}

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

func TestRunOnceLogsAbandonedOnCancel(t *testing.T) {
	dir := t.TempDir()
	writeLabels(t, dir)
	cfg := config.Config{SaturationPolicy: config.SaturationPolicyPassThrough, LabelsDir: dir, TickInterval: time.Second}
	jobs := job.NewManager(60 * time.Second)
	l := NewLoop(cfg, jobs, cancelSolver{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	l.runOnce(context.Background())

	if jobs.Latest() != nil {
		t.Fatal("cancelled window must not publish")
	}
	if got := buf.String(); !strings.Contains(got, "abandoned") {
		t.Fatalf("log should distinguish abandoned window, got %q", got)
	}
}

func TestRunReturnsWhenContextCancelled(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{SaturationPolicy: config.SaturationPolicyPassThrough, LabelsDir: dir, TickInterval: time.Second}
	jobs := job.NewManager(60 * time.Second)
	l := NewLoop(cfg, jobs, okSolver{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
