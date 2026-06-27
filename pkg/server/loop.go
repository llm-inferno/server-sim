package server

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"time"

	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
	"github.com/llm-inferno/server-sim/pkg/job"
)

// Loop drives continuous evaluation windows, one at a time, reading its
// workload from the downward-API labels file.
type Loop struct {
	cfg        config.Config
	jobs       *job.Manager
	cli        solver
	labelsPath string
}

func NewLoop(cfg config.Config, jobs *job.Manager, cli solver) *Loop {
	return &Loop{cfg: cfg, jobs: jobs, cli: cli, labelsPath: labelsFilePath(cfg)}
}

// labelsFilePath returns the path to the downward-API labels file. Shared by
// the continuous loop (NewLoop) and the on-demand /latest path (New).
func labelsFilePath(cfg config.Config) string {
	return filepath.Join(cfg.LabelsDir, "labels")
}

// Run ticks until ctx is cancelled, running one window per tick.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runOnce(ctx)
		}
	}
}

// readProblem reads the current downward-API labels and converts them to a
// ProblemData. ok=false when the pod is not yet ready (labels missing or
// unparseable) — the loop skips, the on-demand /latest path returns 404.
func readProblem(labelsPath string) (evaluator.ProblemData, bool) {
	labels, err := ReadLabels(labelsPath)
	if err != nil {
		return evaluator.ProblemData{}, false
	}
	return LabelsToProblemData(labels)
}

// solveCurrent solves pd under the saturation policy and applies the
// window-averaged offered-load substitution. Shared by the continuous loop and
// the on-demand /latest path so both produce identical envelopes. When the
// evaluator measured a window-averaged offered load (continuous-vllm-server),
// it is reported as the effective offered rate; other backends leave OfferedRPS
// at 0 and the setpoint (or retry-reduced) RPS is preserved.
func solveCurrent(ctx context.Context, cli solver, policy string, pd evaluator.ProblemData) (evaluator.ProblemData, evaluator.AnalysisData, error) {
	eff, ad, err := solveWithPolicy(ctx, cli, policy, pd)
	if err != nil {
		return eff, ad, err
	}
	if ad.OfferedRPS > 0 {
		eff.RPS = ad.OfferedRPS
	}
	return eff, ad, nil
}

// computeLatest reads the current labels and solves on demand, returning the
// envelope fields for the non-continuous /latest path. ok=false ⇒ pod not ready
// (caller returns 404). err != nil ⇒ solve failure (caller returns 500).
func computeLatest(ctx context.Context, policy string, cli solver, labelsPath string) (evaluator.ProblemData, evaluator.AnalysisData, bool, error) {
	pd, ok := readProblem(labelsPath)
	if !ok {
		return evaluator.ProblemData{}, evaluator.AnalysisData{}, false, nil
	}
	eff, ad, err := solveCurrent(ctx, cli, policy, pd)
	if err != nil {
		return eff, ad, false, err
	}
	return eff, ad, true, nil
}

// runOnce executes a single window. It reads the current labels, creates a job,
// runs the window (cancelling it if the allocation concurrency changes
// mid-flight), and stores the effective input + result. Silently skips when the
// pod is not yet ready or the window fails — the Collector handles the
// resulting absence/staleness.
func (l *Loop) runOnce(parent context.Context) {
	pd, ok := readProblem(l.labelsPath)
	if !ok {
		return // not ready
	}

	id := l.jobs.Create()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Watch for an allocation change; cancel the in-flight window so the next
	// window runs under the new concurrency promptly.
	startConc := pd.MaxConcurrency
	go l.watchAllocation(ctx, cancel, startConc)

	eff, result, err := solveCurrent(ctx, l.cli, l.cfg.SaturationPolicy, pd)
	if err != nil {
		l.jobs.Fail(id, err.Error())
		// A cancelled window is the watcher abandoning it on an allocation
		// change, not a solver failure — distinguish the two in the log.
		if errors.Is(err, context.Canceled) {
			log.Printf("loop: window abandoned (allocation changed mid-flight): %v", err)
		} else {
			log.Printf("loop: window failed (skipping publish): %v", err)
		}
		return
	}
	l.jobs.Complete(id, eff, result)
}

// watchAllocation polls the labels file and cancels when maxbatchsize changes
// from startConc. Returns when ctx is done.
func (l *Loop) watchAllocation(ctx context.Context, cancel context.CancelFunc, startConc int) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cur, ok := concurrencyFromFile(l.labelsPath); ok && cur != startConc {
				log.Printf("loop: allocation changed %d -> %d; abandoning in-flight window", startConc, cur)
				cancel()
				return
			}
		}
	}
}
