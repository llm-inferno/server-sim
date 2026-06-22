package main

import (
	"testing"
	"time"
)

func TestScrapeRing_TrailingMeansOverWindow(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	r := newScrapeRing(100)
	// oldest scrape (within window): sum=1.0 count=10
	r.add(metricsScrape{QueueTimeSum: 1.0, QueueTimeCount: 10, InferTimeSum: 5.0, InferTimeCount: 10}, base.Add(-10*time.Second))
	// latest: sum=1.6 count=16  → ΔQ=0.6 over ΔN=6 → 0.1s/req
	r.add(metricsScrape{QueueTimeSum: 1.6, QueueTimeCount: 16, InferTimeSum: 8.0, InferTimeCount: 16}, base)

	q, inf := r.trailingMeans(base, 30*time.Second)
	if q < 0.0999 || q > 0.1001 {
		t.Fatalf("queueMean = %v, want ~0.1", q)
	}
	if inf < 0.4999 || inf > 0.5001 { // ΔInfer=3.0 / ΔN=6 = 0.5
		t.Fatalf("inferMean = %v, want ~0.5", inf)
	}
}

func TestScrapeRing_SingleScrapeReturnsZero(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	r := newScrapeRing(100)
	r.add(metricsScrape{QueueTimeSum: 1.0, QueueTimeCount: 10}, base)
	q, inf := r.trailingMeans(base, 30*time.Second)
	if q != 0 || inf != 0 {
		t.Fatalf("means = (%v,%v), want (0,0) with a single scrape", q, inf)
	}
}
