package main

import (
	"sync"
	"time"
)

// arrivalRing is a mutex-guarded, time-bounded buffer of arrival timestamps. It
// mirrors sampleRing (same retention window + length cap + forward prune) but
// stores only timestamps, since the offered-load average needs a count, not the
// samples themselves. The loop records one timestamp per generated arrival
// (before the concurrency limiter), so count over a window divided by the window
// width gives the window-averaged offered arrival rate — including arrivals the
// limiter drops, which are still offered demand.
type arrivalRing struct {
	mu      sync.Mutex
	entries []time.Time
	window  time.Duration
	maxLen  int
}

func newArrivalRing(window time.Duration, maxLen int) *arrivalRing {
	if maxLen < 1 {
		maxLen = 1
	}
	return &arrivalRing{window: window, maxLen: maxLen, entries: make([]time.Time, 0, maxLen)}
}

// add records an arrival. at must be >= the last add's timestamp: count's forward
// prune loop assumes entries are in non-decreasing time order (true in
// production, where at = time.Now() at arrival generation).
func (r *arrivalRing) add(at time.Time) {
	r.mu.Lock()
	r.entries = append(r.entries, at)
	if len(r.entries) > r.maxLen {
		r.entries = r.entries[len(r.entries)-r.maxLen:]
	}
	r.mu.Unlock()
}

// count returns the number of arrivals in [now-window, now]. window must not
// exceed the ring's retention window (the ring is sized to the longest configured
// trailing window, so any per-solve window fits). Entries older than the
// retention window are pruned to bound memory.
func (r *arrivalRing) count(now time.Time, window time.Duration) int {
	retentionCutoff := now.Add(-r.window)
	queryCutoff := now.Add(-window)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Prune entries older than the retention window (memory bound).
	keep := 0
	for keep < len(r.entries) && r.entries[keep].Before(retentionCutoff) {
		keep++
	}
	if keep > 0 {
		r.entries = r.entries[keep:]
	}
	// Count only the suffix within the (possibly shorter) query window.
	start := 0
	for start < len(r.entries) && r.entries[start].Before(queryCutoff) {
		start++
	}
	return len(r.entries) - start
}
