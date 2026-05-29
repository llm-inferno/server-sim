package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// aggregate folds a windowResult into AnalysisData, applying the
// throughput-capped-at-RPS invariant from the existing evaluators.
func aggregate(r windowResult, pdRPS float32) evaluator.AnalysisData {
	if len(r.Samples) == 0 {
		return evaluator.AnalysisData{}
	}

	var ttftSum, rtSum float64
	var itlMeanSum float64
	var itlMeanCount int
	completed := 0
	for _, s := range r.Samples {
		if s.Failed {
			continue
		}
		completed++
		ttftSum += float64(s.TTFT.Microseconds()) / 1000.0 // ms
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

	avgTTFT := ttftSum / float64(completed)
	avgRT := rtSum / float64(completed)
	var avgITL float64
	if itlMeanCount > 0 {
		avgITL = itlMeanSum / float64(itlMeanCount)
	}

	windowSec := r.WindowEnd.Sub(r.WindowStart).Seconds()
	var throughput float64
	if windowSec > 0 {
		throughput = float64(completed) / windowSec
	}
	if float32(throughput) > pdRPS {
		throughput = float64(pdRPS)
	}

	queueMean := windowDelta(
		r.ScrapeAtStart.QueueTimeSum, r.ScrapeAtEnd.QueueTimeSum,
		r.ScrapeAtStart.QueueTimeCount, r.ScrapeAtEnd.QueueTimeCount,
	) * 1000.0 // sec -> ms

	return evaluator.AnalysisData{
		Throughput:  float32(throughput),
		AvgRespTime: float32(avgRT),
		AvgWaitTime: float32(queueMean),
		AvgTTFT:     float32(avgTTFT),
		AvgITL:      float32(avgITL),
		MaxRPS:      0,
	}
}
