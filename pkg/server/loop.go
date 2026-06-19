package server

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/llm-inferno/server-sim/pkg/config"
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
	return &Loop{cfg: cfg, jobs: jobs, cli: cli, labelsPath: filepath.Join(cfg.LabelsDir, "labels")}
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

// runOnce executes a single window. It reads the current labels, creates a job,
// runs the window (cancelling it if the allocation concurrency changes
// mid-flight), and stores the effective input + result. Silently skips when the
// pod is not yet ready or the window fails — the Collector handles the
// resulting absence/staleness.
func (l *Loop) runOnce(parent context.Context) {
	labels, err := ReadLabels(l.labelsPath)
	if err != nil {
		return // not ready
	}
	pd, ok := LabelsToProblemData(labels)
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

	eff, result, err := solveWithPolicy(ctx, l.cli, l.cfg.SaturationPolicy, pd)
	if err != nil {
		l.jobs.Fail(id, err.Error())
		log.Printf("loop: window failed (skipping publish): %v", err)
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
