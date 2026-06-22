package main

import (
	"sync"
	"time"
)

type ringEntry struct {
	s           sample
	completedAt time.Time
}

// sampleRing is a mutex-guarded, time-bounded buffer of completed requests.
// The ring is sized to the longest configured trailing window (its retention
// window); snapshot then returns only the entries within a caller-supplied
// query window, which may be shorter. A length cap bounds memory at maxLen
// entries under high RPS even within the retention window.
type sampleRing struct {
	mu      sync.Mutex
	entries []ringEntry
	window  time.Duration
	maxLen  int
}

func newSampleRing(window time.Duration, maxLen int) *sampleRing {
	if maxLen < 1 {
		maxLen = 1
	}
	return &sampleRing{window: window, maxLen: maxLen, entries: make([]ringEntry, 0, maxLen)}
}

// add records a completed sample. at must be >= the last add's timestamp:
// snapshot's forward prune loop assumes entries are in non-decreasing time order
// (true in production, where completedAt = time.Now() at completion).
func (r *sampleRing) add(s sample, at time.Time) {
	r.mu.Lock()
	r.entries = append(r.entries, ringEntry{s: s, completedAt: at})
	if len(r.entries) > r.maxLen {
		r.entries = r.entries[len(r.entries)-r.maxLen:]
	}
	r.mu.Unlock()
}

// snapshot returns the samples completed within [now-window, now]. window must
// not exceed the ring's retention window (the ring is sized to the longest
// configured trailing window, so any per-solve window fits). Entries older than
// the retention window are pruned to bound memory; entries are append-ordered
// by time.
func (r *sampleRing) snapshot(now time.Time, window time.Duration) []sample {
	retentionCutoff := now.Add(-r.window)
	queryCutoff := now.Add(-window)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Prune entries older than the retention window (memory bound).
	keep := 0
	for keep < len(r.entries) && r.entries[keep].completedAt.Before(retentionCutoff) {
		keep++
	}
	if keep > 0 {
		r.entries = r.entries[keep:]
	}
	// Return only the suffix within the (possibly shorter) query window.
	start := 0
	for start < len(r.entries) && r.entries[start].completedAt.Before(queryCutoff) {
		start++
	}
	out := make([]sample, len(r.entries)-start)
	for i := start; i < len(r.entries); i++ {
		out[i-start] = r.entries[i].s
	}
	return out
}
