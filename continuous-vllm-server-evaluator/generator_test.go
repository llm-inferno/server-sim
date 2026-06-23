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

// TestRunLoop_CountsOfferedArrivalsIncludingDrops proves the arrival ring records
// offered demand BEFORE the limiter: with rps far above what a 2-slot limiter can
// serve, the loop drops most arrivals, yet they must still be counted as offered.
// So the arrival count must exceed the number of completed samples.
func TestRunLoop_CountsOfferedArrivalsIncludingDrops(t *testing.T) {
	g := newGenerator(nil)
	g.baseURLOverride = "http://fake"
	g.runOne = func(ctx context.Context, _, _ string, _ requestSpec, _ int64) sample {
		time.Sleep(20 * time.Millisecond) // slow service ⇒ limiter stays full ⇒ drops
		return sample{TTFT: time.Millisecond, ResponseTime: time.Millisecond}
	}
	g.live.Store(&liveConfig{
		rps:         500,
		concurrency: 2,
		inSampler:   fixedSampler{v: 8},
		outSampler:  fixedSampler{v: 8},
		servedModel: "m",
		windowSec:   30,
		minSamples:  1,
	})
	g.lim.setLimit(2)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	g.runLoop(ctx)

	now := time.Now()
	offered := g.arrivals.count(now, 30*time.Second)
	served := len(g.ring.snapshot(now, 30*time.Second))
	if offered == 0 {
		t.Fatal("expected the loop to record offered arrivals")
	}
	if offered <= served {
		t.Fatalf("offered arrivals (%d) must exceed served samples (%d): drops are offered load", offered, served)
	}
}

// TestRunLoop_OfferedRateTracksLiveRPS proves the offered-load measurement follows
// the live rate as it changes mid-run, rather than snapping to either endpoint:
// the loop runs at r1 then r2, and the window-averaged offered rate must land
// strictly between the two.
func TestRunLoop_OfferedRateTracksLiveRPS(t *testing.T) {
	const (
		r1     = 100.0
		r2     = 400.0
		phase  = 300 * time.Millisecond
		window = 5 * time.Second
	)
	g := newGenerator(nil)
	g.baseURLOverride = "http://fake"
	g.runOne = func(ctx context.Context, _, _ string, _ requestSpec, _ int64) sample {
		return sample{TTFT: time.Millisecond, ResponseTime: time.Millisecond} // fast ⇒ no drops
	}
	cfg := func(rps float64) *liveConfig {
		return &liveConfig{rps: rps, concurrency: 64, inSampler: fixedSampler{v: 8}, outSampler: fixedSampler{v: 8}, servedModel: "m", windowSec: 30, minSamples: 1}
	}
	g.live.Store(cfg(r1))
	g.lim.setLimit(64)

	go func() {
		time.Sleep(phase)
		g.live.Store(cfg(r2)) // swap to the higher rate halfway through
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*phase)
	defer cancel()
	g.runLoop(ctx)

	elapsed := time.Since(start).Seconds()
	rate := float64(g.arrivals.count(time.Now(), window)) / elapsed
	if rate <= r1 || rate >= r2 {
		t.Fatalf("offered rate = %.1f/s, want strictly between r1=%.0f and r2=%.0f (mid-run rate change)", rate, r1, r2)
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
