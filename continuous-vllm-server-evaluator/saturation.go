package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

const (
	ttftTrendGrowthThreshold = 0.5  // >50% growth from start to end of window
	errorRateThreshold       = 0.05 // >=5%
)

// detectSaturationTrailing flags overload from the trailing-window samples plus
// the queue/inference means. Mirrors the windowed detector's three signals:
// TTFT growth across the window, error rate, and queue-time dominance. Returns
// SaturationNone when fewer than minSamples completed (cannot judge).
func detectSaturationTrailing(samples []sample, minSamples int, queueMeanSec, inferMeanSec float64) string {
	completed := 0
	for _, s := range samples {
		if !s.Failed {
			completed++
		}
	}
	if completed < minSamples {
		return evaluator.SaturationNone
	}
	if errorRate(samples) >= errorRateThreshold {
		return evaluator.SaturationOverload
	}
	if len(samples) >= maxInt(2, minSamples) && ttftGrowth(samples) > ttftTrendGrowthThreshold {
		return evaluator.SaturationOverload
	}
	// Queue dominance: time spent queueing exceeds time spent inferring.
	if inferMeanSec > 0 && queueMeanSec > inferMeanSec {
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
// Failed samples (TTFT=0) are excluded from the regression. endY is evaluated
// at the last *successful* sample's index, not len(samples)-1: trailing failed
// samples must not extrapolate the line past the last real observation (which
// would inflate the growth and trigger a false positive).
// Uses a minimal least-squares slope.
func ttftGrowth(samples []sample) float64 {
	var sumX, sumY, sumXY, sumXX float64
	var n int
	lastX := 0.0
	for i, s := range samples {
		if s.Failed {
			continue
		}
		x := float64(i)
		y := float64(s.TTFT.Milliseconds())
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
		lastX = x
		n++
	}
	if n < 2 {
		return 0
	}
	N := float64(n)
	denom := N*sumXX - sumX*sumX
	if denom == 0 {
		return 0
	}
	slope := (N*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / N
	startY := intercept
	endY := intercept + slope*lastX
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
