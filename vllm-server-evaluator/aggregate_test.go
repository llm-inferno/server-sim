package main

import (
	"testing"
	"time"
)

func TestAggregate_Means(t *testing.T) {
	res := windowResult{
		WindowStart: time.Now().Add(-10 * time.Second),
		WindowEnd:   time.Now(),
		Samples: []sample{
			{TTFT: 100 * time.Millisecond, ITLs: []time.Duration{20 * time.Millisecond, 30 * time.Millisecond}, ResponseTime: 200 * time.Millisecond},
			{TTFT: 120 * time.Millisecond, ITLs: []time.Duration{40 * time.Millisecond, 60 * time.Millisecond}, ResponseTime: 240 * time.Millisecond},
		},
		ScrapeAtStart: metricsScrape{QueueTimeSum: 0, QueueTimeCount: 0},
		ScrapeAtEnd:   metricsScrape{QueueTimeSum: 0.4, QueueTimeCount: 4},
	}
	ad := aggregate(res, 5.0 /*pd.RPS*/)

	// AvgTTFT = (100+120)/2 = 110
	if ad.AvgTTFT < 109 || ad.AvgTTFT > 111 {
		t.Errorf("AvgTTFT = %v, want ~110", ad.AvgTTFT)
	}
	// AvgITL: per-request means {25, 50}, overall mean 37.5
	if ad.AvgITL < 36 || ad.AvgITL > 39 {
		t.Errorf("AvgITL = %v, want ~37.5", ad.AvgITL)
	}
	// AvgRespTime = (200+240)/2 = 220
	if ad.AvgRespTime < 219 || ad.AvgRespTime > 221 {
		t.Errorf("AvgRespTime = %v, want ~220", ad.AvgRespTime)
	}
	// AvgWaitTime = 0.4/4 * 1000 = 100ms
	if ad.AvgWaitTime < 99 || ad.AvgWaitTime > 101 {
		t.Errorf("AvgWaitTime = %v, want ~100", ad.AvgWaitTime)
	}
	// MaxRPS always 0
	if ad.MaxRPS != 0 {
		t.Errorf("MaxRPS = %v, want 0", ad.MaxRPS)
	}
}

func TestAggregate_ThroughputCapAtRPS(t *testing.T) {
	// 100 samples in a 1-second window = 100 RPS measured. pd.RPS = 50.
	// Throughput must be capped at 50 (existing invariant).
	samples := make([]sample, 100)
	for i := range samples {
		samples[i] = sample{TTFT: 100 * time.Millisecond, ResponseTime: 100 * time.Millisecond}
	}
	res := windowResult{
		WindowStart: time.Now().Add(-1 * time.Second),
		WindowEnd:   time.Now(),
		Samples:     samples,
	}
	ad := aggregate(res, 50.0)
	if ad.Throughput > 50.0 {
		t.Errorf("Throughput = %v, want <= 50", ad.Throughput)
	}
}
