package main

import (
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func TestDetectSaturationTrailing_HighErrorRate(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		s := sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond}
		if i < 3 { // 15% failures ≥ 5% threshold
			s.Failed = true
		}
		samples = append(samples, s)
	}
	if got := detectSaturationTrailing(samples, 5, 0.01, 0.05); got != evaluator.SaturationOverload {
		t.Errorf("got %q, want overload (high error rate)", got)
	}
}

func TestDetectSaturationTrailing_QueueDominates(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		samples = append(samples, sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond})
	}
	// queue mean (0.5s) dominates inference mean (0.05s) → overload
	if got := detectSaturationTrailing(samples, 5, 0.5, 0.05); got != evaluator.SaturationOverload {
		t.Errorf("got %q, want overload (queue dominance)", got)
	}
}

func TestDetectSaturationTrailing_Healthy(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		samples = append(samples, sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond})
	}
	if got := detectSaturationTrailing(samples, 5, 0.01, 0.05); got != evaluator.SaturationNone {
		t.Errorf("got %q, want none (healthy)", got)
	}
}

func TestDetectSaturationTrailing_InsufficientSamples(t *testing.T) {
	samples := []sample{{TTFT: time.Second, Failed: true}}
	if got := detectSaturationTrailing(samples, 5, 1.0, 0.01); got != evaluator.SaturationNone {
		t.Errorf("got %q, want none (below minSamples, do not flag)", got)
	}
}

func TestDetectSaturationTrailing_TTFTTrend_TrailingFailuresDoNotInflate(t *testing.T) {
	// 40 successful samples ramp 80ms→119ms: growth at the last successful index
	// (39) is 39/80 = 48.75%, just under the 50% threshold. Two trailing failed
	// samples must NOT extend the extrapolation to index 41 (41/80 = 51.25%),
	// which would be a false positive. Error rate 2/42 ≈ 4.8% stays below 5%, and
	// inferMean=0 disables the queue signal, so only the TTFT trend decides.
	samples := make([]sample, 0, 42)
	for i := 0; i < 40; i++ {
		samples = append(samples, sample{TTFT: time.Duration(80+i) * time.Millisecond})
	}
	samples = append(samples, sample{Failed: true}, sample{Failed: true})
	if got := detectSaturationTrailing(samples, 5, 0, 0); got != evaluator.SaturationNone {
		t.Errorf("got %q, want none (trailing failures must not inflate TTFT growth)", got)
	}
}

func TestDetectSaturationTrailing_QueueDominanceFactorIsOne(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		samples = append(samples, sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond})
	}
	// queueMean (0.08s) is between inferMean (0.05s) and 2*inferMean (0.10s):
	// overload under factor 1.0, NOT under a wrong 2.0x factor.
	if got := detectSaturationTrailing(samples, 5, 0.08, 0.05); got != evaluator.SaturationOverload {
		t.Errorf("got %q, want overload (queue dominance at factor 1.0)", got)
	}
}
