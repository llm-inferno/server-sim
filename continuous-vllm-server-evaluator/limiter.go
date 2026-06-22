package main

import "sync"

// limiter is a live-resizable, drop-if-full concurrency gate. It replaces the
// windowed binary's fixed-size channel semaphore so that maxConcurrency (M*)
// can change between control cycles without restarting the arrival loop.
type limiter struct {
	mu       sync.Mutex
	inflight int
	limit    int
}

func newLimiter(n int) *limiter {
	if n < 1 {
		n = 1
	}
	return &limiter{limit: n}
}

// setLimit changes the cap. Shrinking below current inflight does not cancel
// in-flight requests; they drain naturally and new acquires are refused until
// inflight falls below the new limit.
func (l *limiter) setLimit(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	l.limit = n
	l.mu.Unlock()
}

// tryAcquire reserves a slot if one is free, else returns false (the arrival is
// dropped, exactly as the windowed loop drops on a full semaphore).
func (l *limiter) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight < l.limit {
		l.inflight++
		return true
	}
	return false
}

func (l *limiter) release() {
	l.mu.Lock()
	if l.inflight > 0 {
		l.inflight--
	}
	l.mu.Unlock()
}

func (l *limiter) inFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight
}

func (l *limiter) currentLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}
