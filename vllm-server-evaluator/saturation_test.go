package main

import (
	"testing"
	"time"
)

func mkSamples(ttfts []time.Duration) []sample {
	out := make([]sample, len(ttfts))
	for i, t := range ttfts {
		out[i] = sample{TTFT: t}
	}
	return out
}

func TestDetectSaturation_TTFTTrend_Triggers(t *testing.T) {
	// Linear ramp from 100ms to 200ms (>50% growth).
	ttfts := []time.Duration{
		100 * time.Millisecond, 110 * time.Millisecond, 130 * time.Millisecond,
		150 * time.Millisecond, 175 * time.Millisecond, 200 * time.Millisecond,
	}
	res := windowResult{Samples: mkSamples(ttfts)}
	got := detectSaturation(res, 0)
	if got != "overloaded" {
		t.Errorf("got %q, want overloaded (TTFT trend)", got)
	}
}

func TestDetectSaturation_TTFTTrend_StableNoTrigger(t *testing.T) {
	ttfts := []time.Duration{
		100 * time.Millisecond, 105 * time.Millisecond, 102 * time.Millisecond,
		98 * time.Millisecond, 103 * time.Millisecond, 100 * time.Millisecond,
	}
	res := windowResult{Samples: mkSamples(ttfts)}
	if got := detectSaturation(res, 0); got != "" {
		t.Errorf("got %q, want empty (stable TTFT)", got)
	}
}

func TestDetectSaturation_QueueDominance(t *testing.T) {
	res := windowResult{
		Samples: mkSamples([]time.Duration{100 * time.Millisecond, 100 * time.Millisecond}),
		ScrapeAtStart: metricsScrape{QueueTimeSum: 0, QueueTimeCount: 0, InferTimeSum: 0, InferTimeCount: 0},
		ScrapeAtEnd:   metricsScrape{QueueTimeSum: 30, QueueTimeCount: 10, InferTimeSum: 10, InferTimeCount: 10},
	}
	if got := detectSaturation(res, 0); got != "overloaded" {
		t.Errorf("got %q, want overloaded (queue > infer)", got)
	}
}

func TestDetectSaturation_TTFTTrend_IgnoresFailedSamples(t *testing.T) {
	// 20 stable successful samples with one failed sample interspersed.
	// Error rate = 1/21 ≈ 4.8% (below the 5% threshold), so only the TTFT
	// trend signal is relevant. The failed sample's TTFT=0 must not pull the
	// regression line and produce a false positive.
	samples := make([]sample, 21)
	ttfts := []time.Duration{
		100, 102, 101, 99, 100, 103, 98, 101, 100, 102,
		101, 99, 100, 103, 98, 101, 100, 102, 101, 99,
	}
	j := 0
	for i := range samples {
		if i == 10 {
			samples[i] = sample{Failed: true, StatusCode: 0}
		} else {
			samples[i] = sample{TTFT: ttfts[j] * time.Millisecond}
			j++
		}
	}
	res := windowResult{Samples: samples}
	if got := detectSaturation(res, 0); got != "" {
		t.Errorf("got %q, want empty (failed sample should be ignored, TTFT is stable)", got)
	}
}

func TestDetectSaturation_ErrorRate(t *testing.T) {
	samples := []sample{
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{Failed: true, StatusCode: 429},
	}
	res := windowResult{Samples: samples}
	if got := detectSaturation(res, 0); got != "overloaded" {
		t.Errorf("got %q, want overloaded (error rate)", got)
	}
}
