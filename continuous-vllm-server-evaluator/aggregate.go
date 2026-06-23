package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// aggregateTrailing folds the trailing-window samples (+ a queue-time mean from
// the scrape ring) into AnalysisData. Means are in ms; throughput is completed
// requests over the trailing window width, capped at offeredRPS — the offered
// arrival rate averaged over the SAME window (not the instantaneous setpoint),
// so the goodput ≤ offered invariant is enforced against a consistent λ.
// offeredRPS is also reported back on AnalysisData.OfferedRPS so the collector
// pairs window-averaged metrics with a window-averaged offered load.
func aggregateTrailing(samples []sample, offeredRPS float64, windowSec float64, queueMeanSec float64) evaluator.AnalysisData {
	if len(samples) == 0 {
		return evaluator.AnalysisData{}
	}
	var ttftSum, rtSum, itlMeanSum float64
	var itlMeanCount, completed int
	for _, s := range samples {
		if s.Failed {
			continue
		}
		completed++
		ttftSum += float64(s.TTFT.Microseconds()) / 1000.0
		rtSum += float64(s.ResponseTime.Microseconds()) / 1000.0
		if len(s.ITLs) > 0 {
			var itlSum float64
			for _, itl := range s.ITLs {
				itlSum += float64(itl.Microseconds()) / 1000.0
			}
			itlMeanSum += itlSum / float64(len(s.ITLs))
			itlMeanCount++
		}
	}
	if completed == 0 {
		return evaluator.AnalysisData{}
	}
	avgITL := 0.0
	if itlMeanCount > 0 {
		avgITL = itlMeanSum / float64(itlMeanCount)
	}
	throughput := 0.0
	if windowSec > 0 {
		throughput = float64(completed) / windowSec
	}
	// Cap goodput at the window-averaged offered load. Only when offeredRPS was
	// measured (> 0): a 0 means "not measured" and must not zero out a real
	// throughput. Post-warmup every counted arrival also yields a sample, so
	// offeredRPS ≥ throughput holds and the cap rarely binds (it guards boundary
	// effects, e.g. requests that completed in-window but arrived before it).
	if offeredRPS > 0 && throughput > offeredRPS {
		throughput = offeredRPS
	}
	// NOTE: queue/inference means come from the trailing scrape-ring delta over the
	// observation window — intentionally different from the windowed baseline's
	// per-window start/end /metrics bookends. AvgWaitTime is therefore not directly
	// comparable to the baseline; attribute differences to the continuous model.
	return evaluator.AnalysisData{
		Throughput:  float32(throughput),
		AvgRespTime: float32(rtSum / float64(completed)),
		AvgWaitTime: float32(queueMeanSec * 1000.0),
		AvgTTFT:     float32(ttftSum / float64(completed)),
		AvgITL:      float32(avgITL),
		MaxRPS:      0, // vllm-server does not compute MaxRPS (pass-through policy)
		OfferedRPS:  float32(offeredRPS),
	}
}
