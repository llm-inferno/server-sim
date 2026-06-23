package main

import (
	"testing"
	"time"
)

func TestArrivalRing_CountPrunesOldEntries(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newArrivalRing(10*time.Second, 1000)
	r.add(base.Add(-20 * time.Second)) // outside window
	r.add(base.Add(-5 * time.Second))  // inside
	r.add(base.Add(-1 * time.Second))  // inside

	if got := r.count(base, 10*time.Second); got != 2 {
		t.Fatalf("count = %d, want 2 (10s window)", got)
	}
	// A later now pushes the -5s entry out of the window.
	if got := r.count(base.Add(7*time.Second), 10*time.Second); got != 1 {
		t.Fatalf("second count = %d, want 1 (-5s entry outside 10s window)", got)
	}
}

func TestArrivalRing_CountHonorsQueryWindow(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newArrivalRing(60*time.Second, 1000) // retain up to 60s
	r.add(base.Add(-50 * time.Second))        // inside 60s retention, outside 30s query
	r.add(base.Add(-20 * time.Second))        // inside both
	r.add(base.Add(-5 * time.Second))         // inside both

	if got := r.count(base, 30*time.Second); got != 2 {
		t.Fatalf("count(30s) = %d, want 2", got)
	}
	if got := r.count(base, 60*time.Second); got != 3 {
		t.Fatalf("count(60s) = %d, want 3", got)
	}
}

func TestArrivalRing_HardCapEvictsOldest(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newArrivalRing(time.Hour, 3) // cap 3, generous window
	for i := 0; i < 5; i++ {
		r.add(base.Add(time.Duration(i) * time.Millisecond))
	}
	// Only the 3 newest survive the hard cap; all are within the 1h window.
	if got := r.count(base.Add(time.Second), time.Hour); got != 3 {
		t.Fatalf("count = %d, want 3 (hard cap)", got)
	}
}
