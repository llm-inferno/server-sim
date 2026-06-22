package main

import (
	"sync"
	"testing"
)

func TestLimiter_AcquireUpToLimitThenDrops(t *testing.T) {
	l := newLimiter(2)
	if !l.tryAcquire() || !l.tryAcquire() {
		t.Fatal("first two acquires should succeed")
	}
	if l.tryAcquire() {
		t.Fatal("third acquire should be dropped at limit 2")
	}
	if l.inFlight() != 2 {
		t.Fatalf("inFlight = %d, want 2", l.inFlight())
	}
	l.release()
	if !l.tryAcquire() {
		t.Fatal("acquire after release should succeed")
	}
}

func TestLimiter_SetLimitGrowsAndShrinks(t *testing.T) {
	l := newLimiter(1)
	if !l.tryAcquire() || l.tryAcquire() {
		t.Fatal("limit 1: first ok, second dropped")
	}
	l.setLimit(3) // grow
	if !l.tryAcquire() || !l.tryAcquire() {
		t.Fatal("after grow to 3, two more acquires should succeed")
	}
	if l.tryAcquire() {
		t.Fatal("now at 3 inflight, should drop")
	}
	l.setLimit(1) // shrink below inflight; existing holders drain naturally
	if l.currentLimit() != 1 {
		t.Fatalf("currentLimit = %d, want 1", l.currentLimit())
	}
	if l.tryAcquire() {
		t.Fatal("inflight(3) >= limit(1): must drop")
	}
}

func TestLimiter_ConcurrentSafe(t *testing.T) {
	l := newLimiter(8)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.tryAcquire() {
				l.release()
			}
		}()
	}
	wg.Wait()
	if l.inFlight() != 0 {
		t.Fatalf("inFlight = %d, want 0 after balanced acquire/release", l.inFlight())
	}
}
