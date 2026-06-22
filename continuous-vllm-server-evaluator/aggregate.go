package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// aggregateTrailing folds the trailing-window samples (+ a queue-time mean from
// the scrape ring) into AnalysisData. Means are in ms; throughput is completed
// requests over the trailing window width, capped at the offered RPS.
func aggregateTrailing(samples []sample, pdRPS float32, windowSec float64, queueMeanSec float64) evaluator.AnalysisData {
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
	if float32(throughput) > pdRPS {
		throughput = float64(pdRPS)
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
	}
}
