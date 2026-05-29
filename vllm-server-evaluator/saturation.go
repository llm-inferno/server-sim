package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

const (
	ttftTrendGrowthThreshold = 0.5  // >50% growth from start to end of window
	errorRateThreshold       = 0.05 // >=5%
)

// detectSaturation evaluates three independent signals; if any triggers,
// returns evaluator.SaturationOverload. Otherwise returns "".
//
// minSamples avoids spurious trend detection on tiny windows.
func detectSaturation(r windowResult, minSamples int) string {
	// Signal 3: error rate (cheapest, do first).
	if rate := errorRate(r.Samples); rate >= errorRateThreshold {
		return evaluator.SaturationOverload
	}

	// Signal 1: TTFT trend.
	if len(r.Samples) >= maxInt(2, minSamples) && ttftGrowth(r.Samples) > ttftTrendGrowthThreshold {
		return evaluator.SaturationOverload
	}

	// Signal 2: queue dominance from /metrics deltas.
	queueMean := windowDelta(r.ScrapeAtStart.QueueTimeSum, r.ScrapeAtEnd.QueueTimeSum,
		r.ScrapeAtStart.QueueTimeCount, r.ScrapeAtEnd.QueueTimeCount)
	inferMean := windowDelta(r.ScrapeAtStart.InferTimeSum, r.ScrapeAtEnd.InferTimeSum,
		r.ScrapeAtStart.InferTimeCount, r.ScrapeAtEnd.InferTimeCount)
	if inferMean > 0 && queueMean > inferMean {
		return evaluator.SaturationOverload
	}

	return evaluator.SaturationNone
}

func errorRate(samples []sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	failed := 0
	for _, s := range samples {
		if s.Failed {
			failed++
		}
	}
	return float64(failed) / float64(len(samples))
}

// ttftGrowth fits a line over the TTFT-by-index series and returns the
// fractional growth from start to end:  (end_y - start_y) / start_y.
// Uses a minimal least-squares slope.
func ttftGrowth(samples []sample) float64 {
	n := len(samples)
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, s := range samples {
		x := float64(i)
		y := float64(s.TTFT.Milliseconds())
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	N := float64(n)
	denom := N*sumXX - sumX*sumX
	if denom == 0 {
		return 0
	}
	slope := (N*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / N
	startY := intercept
	endY := intercept + slope*float64(n-1)
	if startY <= 0 {
		return 0
	}
	return (endY - startY) / startY
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
