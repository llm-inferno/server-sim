package main

import (
	"sync"
	"time"
)

type scrapeEntry struct {
	m  metricsScrape
	at time.Time
}

// scrapeRing keeps a short history of timestamped /metrics scrapes so queue and
// inference time can be averaged over the trailing window [now-window, now] —
// the same window the client-side sample ring reports over.
type scrapeRing struct {
	mu      sync.Mutex
	entries []scrapeEntry
	maxLen  int
}

func newScrapeRing(maxLen int) *scrapeRing {
	if maxLen < 2 {
		maxLen = 2
	}
	return &scrapeRing{maxLen: maxLen}
}

func (r *scrapeRing) add(m metricsScrape, at time.Time) {
	r.mu.Lock()
	r.entries = append(r.entries, scrapeEntry{m: m, at: at})
	if len(r.entries) > r.maxLen {
		r.entries = r.entries[len(r.entries)-r.maxLen:]
	}
	r.mu.Unlock()
}

// trailingMeans deltas the latest scrape against the oldest scrape at or after
// (now-window), giving per-completion queue/inference means in seconds. Returns
// (0,0) when there are fewer than two scrapes.
func (r *scrapeRing) trailingMeans(now time.Time, window time.Duration) (queueMeanSec, inferMeanSec float64) {
	cutoff := now.Add(-window)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) < 2 {
		return 0, 0
	}
	latest := r.entries[len(r.entries)-1].m
	// oldest entry whose timestamp is >= cutoff; fall back to the first entry.
	oldest := r.entries[0].m
	for i := 0; i < len(r.entries); i++ {
		if !r.entries[i].at.Before(cutoff) {
			oldest = r.entries[i].m
			break
		}
	}
	queueMeanSec = windowDelta(oldest.QueueTimeSum, latest.QueueTimeSum, oldest.QueueTimeCount, latest.QueueTimeCount)
	inferMeanSec = windowDelta(oldest.InferTimeSum, latest.InferTimeSum, oldest.InferTimeCount, latest.InferTimeCount)
	return queueMeanSec, inferMeanSec
}
