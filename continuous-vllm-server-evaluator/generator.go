package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

type liveConfig struct {
	rps         float64
	concurrency int
	inSampler   tokenSampler
	outSampler  tokenSampler
	ignoreEOS   bool
	servedModel string
	queueMetric string
	minSamples  int
	windowSec   float64
}

type generator struct {
	live    atomic.Pointer[liveConfig]
	pairing atomic.Pointer[pairingState]
	lim     *limiter
	ring    *sampleRing
	scrapes *scrapeRing
	lookup  map[string]serverConfig

	baseURLOverride string        // test hook; empty in production
	warmup          time.Duration // one-time warmup, anchored at the first accepted arrival; samples completing before it are dropped

	// Injectable for tests; wired to runOneRequest / scrapeMetrics in production.
	runOne func(ctx context.Context, baseURL, model string, spec requestSpec, seed int64) sample
	scrape func(ctx context.Context, url, queueMetric string) (metricsScrape, error)
}

func newGenerator(lookup map[string]serverConfig) *generator {
	// Size the ring to the longest configured trailing window so a /solve for any
	// entry can read its full window; shorter windows are filtered in snapshot.
	retain := 30 * time.Second
	for _, sc := range lookup {
		if w := time.Duration(sc.TrailingWindowSec) * time.Second; w > retain {
			retain = w
		}
	}
	return &generator{
		lim:     newLimiter(evaluator.DefaultMaxConcurrency),
		ring:    newSampleRing(retain, 200_000),
		scrapes: newScrapeRing(256),
		lookup:  lookup,
		runOne:  runOneRequest,
		scrape:  scrapeMetrics,
	}
}

func (g *generator) baseURL() string {
	if g.baseURLOverride != "" {
		return g.baseURLOverride
	}
	ps := g.pairing.Load()
	if ps == nil || ps.VLLMPodIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ps.VLLMPodIP, ps.VLLMPort)
}

// runLoop issues Poisson arrivals at the live RPS until ctx is cancelled. It is
// the single owner of the arrival RNG (one goroutine), spawning a bounded
// request goroutine per accepted arrival.
func (g *generator) runLoop(ctx context.Context) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // one RNG owned by this loop goroutine; never shared with request goroutines
	var seed int64
	var warmupEnd time.Time // set at the first accepted arrival (see below); samples completing before it are dropped

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cfg := g.live.Load()
		base := g.baseURL()
		if cfg == nil || cfg.rps <= 0 || base == "" {
			// Not yet configured / paired: idle briefly and re-check.
			select {
			case <-ctx.Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}

		gap := time.Duration(rng.ExpFloat64() / cfg.rps * float64(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(gap):
		}

		if !g.lim.tryAcquire() {
			continue // drop excess arrival, exactly like the windowed semaphore
		}
		// Anchor the one-time warmup window at the first accepted arrival, i.e.
		// when traffic actually begins — not at loop start. The loop is spun up
		// at process start but idles (above) until it is both configured and
		// paired, which can take longer than warmup; anchoring at start would
		// let the window elapse during that idle wait and drop nothing.
		if warmupEnd.IsZero() {
			warmupEnd = time.Now().Add(g.warmup)
		}
		dropBefore := warmupEnd // capture by value; never mutated after the first arrival
		spec := requestSpec{
			InputTokens:  cfg.inSampler.Sample(rng),
			OutputTokens: cfg.outSampler.Sample(rng),
			IgnoreEOS:    cfg.ignoreEOS,
		}
		seed++
		reqSeed := seed
		go func() {
			defer g.lim.release()
			s := g.runOne(ctx, base, cfg.servedModel, spec, reqSeed)
			now := time.Now()
			if now.Before(dropBefore) {
				return // drop warmup-phase samples
			}
			g.ring.add(s, now)
		}()
	}
}
