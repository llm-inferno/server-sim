package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLoop_FillsRingAndRespectsLimiter(t *testing.T) {
	g := newGenerator(nil)
	g.baseURLOverride = "http://fake"
	var maxObserved int64
	// Stub request: records peak inflight, sleeps briefly, returns a sample.
	g.runOne = func(ctx context.Context, baseURL, model string, spec requestSpec, seed int64) sample {
		if n := int64(g.lim.inFlight()); n > atomic.LoadInt64(&maxObserved) {
			atomic.StoreInt64(&maxObserved, n)
		}
		time.Sleep(20 * time.Millisecond)
		return sample{TTFT: 5 * time.Millisecond, ResponseTime: 10 * time.Millisecond}
	}
	g.live.Store(&liveConfig{
		rps:         200,
		concurrency: 4,
		inSampler:   fixedSampler{v: 16},
		outSampler:  fixedSampler{v: 8},
		servedModel: "m",
		windowSec:   30,
		minSamples:  1,
	})
	g.lim.setLimit(4)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	g.runLoop(ctx)

	got := g.ring.snapshot(time.Now(), 30*time.Second)
	if len(got) == 0 {
		t.Fatal("expected the loop to record completed samples")
	}
	if maxObserved > 4 {
		t.Fatalf("peak inflight = %d, must not exceed limit 4", maxObserved)
	}
}

func TestNewGenerator_RingRetainsLongestTrailingWindow(t *testing.T) {
	lookup := map[string]serverConfig{
		"H100|a": {TrailingWindowSec: 30},
		"H100|b": {TrailingWindowSec: 90},
	}
	g := newGenerator(lookup)
	base := time.Unix(1_000_000, 0)
	// An 80s-old sample is inside the longest configured window (90s) and must
	// survive in the ring; a ring hardcoded to 30s would prune it.
	g.ring.add(sample{TTFT: 1}, base.Add(-80*time.Second))
	if got := g.ring.snapshot(base, 90*time.Second); len(got) != 1 {
		t.Fatalf("ring dropped a sample inside the 90s window: len=%d, want 1", len(got))
	}
}

// TestRunLoop_WarmupAnchorsAtFirstArrival proves the warmup window starts when
// traffic begins, not at loop start. The loop idles unconfigured for 150ms,
// then a config is stored; the warmup (450ms) then extends past the ctx
// deadline, so every post-config sample falls inside warmup and is dropped —
// leaving the ring empty even though requests demonstrably ran. If warmup were
// anchored at loop start (the old bug), its 450ms window would end at ~450ms
// while traffic ran 150–500ms, so samples after 450ms would be recorded and
// the ring would be non-empty.
func TestRunLoop_WarmupAnchorsAtFirstArrival(t *testing.T) {
	g := newGenerator(nil)
	g.baseURLOverride = "http://fake"
	g.warmup = 450 * time.Millisecond
	var called int64
	g.runOne = func(ctx context.Context, _, _ string, _ requestSpec, _ int64) sample {
		atomic.AddInt64(&called, 1)
		return sample{TTFT: time.Millisecond, ResponseTime: time.Millisecond}
	}

	// The loop stays idle until configured, so the first accepted arrival cannot
	// precede this store — making it the warmup anchor point.
	go func() {
		time.Sleep(150 * time.Millisecond)
		g.live.Store(&liveConfig{
			rps:         200,
			concurrency: 8,
			inSampler:   fixedSampler{v: 4},
			outSampler:  fixedSampler{v: 4},
			servedModel: "m",
			windowSec:   30,
			minSamples:  1,
		})
		g.lim.setLimit(8)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	g.runLoop(ctx)

	if atomic.LoadInt64(&called) == 0 {
		t.Fatal("expected requests to run after config was stored")
	}
	if got := g.ring.snapshot(time.Now(), 30*time.Second); len(got) != 0 {
		t.Fatalf("ring has %d samples; warmup anchored at first arrival should drop all traffic within the deadline", len(got))
	}
}

func TestRunLoop_IdlesWhenUnconfigured(t *testing.T) {
	g := newGenerator(nil)
	g.baseURLOverride = "http://fake"
	called := int64(0)
	g.runOne = func(ctx context.Context, _, _ string, _ requestSpec, _ int64) sample {
		atomic.AddInt64(&called, 1)
		return sample{}
	}
	// No live config stored → rps unknown → loop must not fire requests.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	g.runLoop(ctx)
	if atomic.LoadInt64(&called) != 0 {
		t.Fatalf("runOne called %d times with no config; want 0", called)
	}
}
