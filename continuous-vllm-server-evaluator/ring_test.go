package main

import (
	"testing"
	"time"
)

func TestSampleRing_SnapshotPrunesOldEntries(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newSampleRing(10*time.Second, 1000)
	r.add(sample{TTFT: 1}, base.Add(-20*time.Second)) // outside window
	r.add(sample{TTFT: 2}, base.Add(-5*time.Second))  // inside
	r.add(sample{TTFT: 3}, base.Add(-1*time.Second))  // inside

	got := r.snapshot(base, 10*time.Second)
	if len(got) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (10s window)", len(got))
	}
	// A second snapshot with later now pushes the -5s entry out of the window.
	if again := r.snapshot(base.Add(7*time.Second), 10*time.Second); len(again) != 1 {
		t.Fatalf("second snapshot len = %d, want 1 (-5s entry outside 10s window)", len(again))
	}
}

func TestSampleRing_SnapshotHonorsQueryWindow(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newSampleRing(60*time.Second, 1000) // retain up to 60s
	r.add(sample{TTFT: 1}, base.Add(-50*time.Second)) // inside 60s retention, outside 30s query
	r.add(sample{TTFT: 2}, base.Add(-20*time.Second)) // inside both
	r.add(sample{TTFT: 3}, base.Add(-5*time.Second))  // inside both

	// A 30s query window must exclude the -50s entry even though the ring retains it.
	if got := r.snapshot(base, 30*time.Second); len(got) != 2 {
		t.Fatalf("snapshot(30s) len = %d, want 2", len(got))
	}
	// The full 60s retention window includes all three.
	if got := r.snapshot(base, 60*time.Second); len(got) != 3 {
		t.Fatalf("snapshot(60s) len = %d, want 3", len(got))
	}
}

func TestSampleRing_HardCapEvictsOldest(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newSampleRing(time.Hour, 3) // cap 3, generous window
	for i := 0; i < 5; i++ {
		r.add(sample{StatusCode: i}, base.Add(time.Duration(i)*time.Millisecond))
	}
	got := r.snapshot(base.Add(time.Second), time.Hour)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (hard cap)", len(got))
	}
	if got[0].StatusCode != 2 {
		t.Fatalf("oldest retained StatusCode = %d, want 2 (0,1 evicted)", got[0].StatusCode)
	}
}
