package main

import (
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func TestAggregateTrailing_Means(t *testing.T) {
	samples := []sample{
		{TTFT: 100 * time.Millisecond, ITLs: []time.Duration{20 * time.Millisecond, 30 * time.Millisecond}, ResponseTime: 200 * time.Millisecond},
		{TTFT: 120 * time.Millisecond, ITLs: []time.Duration{40 * time.Millisecond, 60 * time.Millisecond}, ResponseTime: 240 * time.Millisecond},
	}
	ad := aggregateTrailing(samples, 5.0 /*pdRPS*/, 10.0 /*windowSec*/, 0.1 /*queueMeanSec*/)

	if ad.AvgTTFT < 109 || ad.AvgTTFT > 111 { // (100+120)/2
		t.Errorf("AvgTTFT = %v, want ~110", ad.AvgTTFT)
	}
	if ad.AvgITL < 36 || ad.AvgITL > 39 { // mean of per-req ITL means: (25+50)/2=37.5
		t.Errorf("AvgITL = %v, want ~37.5", ad.AvgITL)
	}
	if ad.AvgWaitTime < 99 || ad.AvgWaitTime > 101 { // 0.1s -> 100ms
		t.Errorf("AvgWaitTime = %v, want ~100", ad.AvgWaitTime)
	}
	// throughput = 2 completed / 10s = 0.2, below RPS cap (5) → 0.2
	if ad.Throughput < 0.19 || ad.Throughput > 0.21 {
		t.Errorf("Throughput = %v, want ~0.2", ad.Throughput)
	}
}

func TestAggregateTrailing_ThroughputCappedAtRPS(t *testing.T) {
	samples := make([]sample, 100)
	for i := range samples {
		samples[i] = sample{TTFT: 10 * time.Millisecond, ResponseTime: 10 * time.Millisecond}
	}
	// 100 completed / 1s = 100, but RPS cap = 5
	ad := aggregateTrailing(samples, 5.0, 1.0, 0)
	if ad.Throughput != 5.0 {
		t.Errorf("Throughput = %v, want 5.0 (capped at RPS)", ad.Throughput)
	}
}

func TestAggregateTrailing_Empty(t *testing.T) {
	ad := aggregateTrailing(nil, 5.0, 10.0, 0)
	if ad != (evaluator.AnalysisData{}) {
		t.Errorf("empty samples → zero AnalysisData, got %+v", ad)
	}
}
