# Continuous (Non-Windowed) vLLM Evaluator — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a new server-sim evaluator binary `continuous-vllm-server-evaluator` whose `/solve` reconfigures a *persistent* arrival loop (it never stops generating traffic against the paired vLLM) and returns metrics aggregated over a fixed **trailing window of the last `n` seconds**, replacing the existing per-call windowed `runWindow` model.

**Architecture:** A single long-lived goroutine (`generator.runLoop`) issues Poisson arrivals against the paired vLLM, bounded by a **resizable** concurrency limiter, recording each completed request into a time-bounded **ring buffer**. `/solve` is a lightweight RPC: it atomically swaps the live config (RPS, token samplers, concurrency) from the incoming `ProblemData`, scrapes vLLM `/metrics`, and aggregates the trailing-window samples (+ a `/metrics` delta) into `AnalysisData`. The binary is a self-contained `package main` directory that **copies** the stable primitives (request/SSE, token distributions, `/metrics` scrape, pairing, config) from `vllm-server-evaluator/`; the existing windowed binary is left byte-for-byte unchanged as the A/B baseline.

**Tech Stack:** Go 1.25, `github.com/gin-gonic/gin`, `k8s.io/client-go`, stdlib `testing` + `net/http/httptest` + `k8s.io/client-go/kubernetes/fake`. No new module dependencies.

## Global Constraints

- Module path: `github.com/llm-inferno/server-sim`; Go `1.25.0`. (`go.mod`)
- Wire contract is unchanged: `POST /solve` accepts `evaluator.ProblemData`, returns `evaluator.AnalysisData` (`pkg/evaluator/types.go`). server-sim's loop and the existing `vllm-server-evaluator/` are **not modified** by this plan.
- Tests use stdlib `testing` only — **no testify/ginkgo**. Fake vLLM via `net/http/httptest`; fake k8s via `k8s.io/client-go/kubernetes/fake`. Match the existing patterns in `vllm-server-evaluator/*_test.go`.
- `Throughput ≤ RPS` invariant must hold (cap at offered RPS), matching `aggregate` in the existing binary.
- All new code lives under `continuous-vllm-server-evaluator/` (package `main`), except additive lines in `Dockerfile.evaluator` and `evaluator.sh`.
- Commit after every task. Run `gofmt`/`go vet ./continuous-vllm-server-evaluator/` before each commit.

---

### Task 1: Scaffold directory + copy frozen primitives

Establish the new package and copy the stable, behavior-frozen files (and their tests) verbatim so the package compiles and the copied tests pass — proving the copies are intact before any new logic is added.

**Files:**
- Create `continuous-vllm-server-evaluator/types.go` — copy `requestSpec`, `sample`, `metricsScrape` from `vllm-server-evaluator/types.go` (DROP `windowResult`; it is windowed-only).
- Create `continuous-vllm-server-evaluator/request.go` — copy `completionsRequest`, `runOneRequest`, `timeSince` from `vllm-server-evaluator/generator.go`.
- Create `continuous-vllm-server-evaluator/distribution.go` — copy `vllm-server-evaluator/distribution.go` verbatim.
- Create `continuous-vllm-server-evaluator/metrics.go` — copy `scrapeMetrics`, `parseValue`, `windowDelta` from `vllm-server-evaluator/metrics.go` verbatim.
- Create `continuous-vllm-server-evaluator/pairing.go` — copy `vllm-server-evaluator/pairing.go` verbatim.
- Create `continuous-vllm-server-evaluator/prompt.go` — copy `vllm-server-evaluator/prompt.go` verbatim.
- Create `continuous-vllm-server-evaluator/config.go` — copy `vllm-server-evaluator/config.go`, then **add** a `TrailingWindowSec int` field (see code) to both `configEntry` (json `trailingWindowSec`) and `serverConfig`, defaulting to `30` when `≤ 0`.
- Create `continuous-vllm-server-evaluator/{distribution,metrics,pairing,prompt,config}_test.go` — copy the corresponding existing tests verbatim (they exercise the copied code).

**Interfaces:**
- Produces (for later tasks): `requestSpec{InputTokens, OutputTokens int; IgnoreEOS bool}`, `sample{StartedAt time.Time; TTFT time.Duration; ITLs []time.Duration; ResponseTime time.Duration; Failed bool; StatusCode int}`, `metricsScrape{QueueTimeSum, QueueTimeCount, InferTimeSum, InferTimeCount float64}`; `runOneRequest(ctx context.Context, vllmBaseURL, model string, spec requestSpec, seed int64) sample`; `newSampler(kind string, avg int) (tokenSampler, error)` returning `tokenSampler` (`Sample(*rand.Rand) int`); `scrapeMetrics(ctx, url, queueTimeMetric string) (metricsScrape, error)`; `windowDelta(start, end, startCount, endCount float64) float64`; `resolvePairing(ctx, client kubernetes.Interface, port int) (*pairingState, error)` with `pairingState{PairID, VLLMNamespace, VLLMDeployment, VLLMPodIP string; VLLMPort int}`; `loadConfig() (map[string]serverConfig, error)` with `serverConfig` now carrying `TrailingWindowSec int`.

- [ ] **Step 1: Create the directory and copy the frozen files**

Copy the files listed above. The only edit during copy is to `config.go` — add the trailing-window field:

```go
// in configEntry:
	TrailingWindowSec int `json:"trailingWindowSec"` // width of the trailing observation window in seconds; ≤0 → 30

// in serverConfig:
	TrailingWindowSec int // ≤0 → 30 at resolution time

// in loadConfig(), inside the per-entry mapping (alongside the other fields):
		tw := e.TrailingWindowSec
		if tw <= 0 {
			tw = 30
		}
		lookup[e.Accelerator+"|"+e.Model] = serverConfig{
			// ... existing fields copied verbatim ...
			TrailingWindowSec: tw,
		}
```

- [ ] **Step 2: Verify the package compiles**

Run: `cd /Users/tantawi/Projects/llm-inferno/server-sim && go build ./continuous-vllm-server-evaluator/`
Expected: builds with no errors. (Some copied funcs are as-yet unused; Go permits unused package-level functions.)

- [ ] **Step 3: Run the copied tests**

Run: `go test ./continuous-vllm-server-evaluator/ -run 'Distribution|Metrics|Pairing|Prompt|Config' -v`
Expected: PASS (the copies behave identically to the originals).

- [ ] **Step 4: Add a config test for the new trailing-window default**

Add to `config_test.go`:

```go
func TestLoadConfig_TrailingWindowDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"configs":[{"accelerator":"H100","model":"m","vllmServedModelName":"m","minSamples":5}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)
	lookup, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := lookup["H100|m"].TrailingWindowSec; got != 30 {
		t.Errorf("TrailingWindowSec = %d, want default 30", got)
	}
}
```

- [ ] **Step 5: Run and verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestLoadConfig_TrailingWindowDefault -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add continuous-vllm-server-evaluator/
git commit -m "feat(continuous-eval): scaffold dir, copy frozen primitives + trailing-window config"
```

---

### Task 2: Resizable concurrency limiter

A live-resizable replacement for the windowed binary's fixed channel semaphore. Drop-if-full semantics identical to today's `select { case sem<-{}: default: continue }`.

**Files:**
- Create `continuous-vllm-server-evaluator/limiter.go`
- Test `continuous-vllm-server-evaluator/limiter_test.go`

**Interfaces:**
- Produces: `type limiter`; `newLimiter(n int) *limiter`; `(*limiter) setLimit(n int)`; `(*limiter) tryAcquire() bool`; `(*limiter) release()`; `(*limiter) inFlight() int`; `(*limiter) currentLimit() int`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestLimiter -v`
Expected: FAIL — `undefined: newLimiter`.

- [ ] **Step 3: Write the implementation**

```go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestLimiter -race -v`
Expected: PASS (including `-race`).

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/limiter.go continuous-vllm-server-evaluator/limiter_test.go
git commit -m "feat(continuous-eval): resizable drop-if-full concurrency limiter"
```

---

### Task 3: Trailing-window sample ring

A time-bounded buffer of completed requests. `snapshot(now)` returns samples completed within the trailing window and prunes older ones; a hard count cap bounds memory under high RPS.

**Files:**
- Create `continuous-vllm-server-evaluator/ring.go`
- Test `continuous-vllm-server-evaluator/ring_test.go`

**Interfaces:**
- Consumes: `sample` (Task 1).
- Produces: `type sampleRing`; `newSampleRing(window time.Duration, maxLen int) *sampleRing`; `(*sampleRing) add(s sample, at time.Time)`; `(*sampleRing) snapshot(now time.Time) []sample`.

- [ ] **Step 1: Write the failing test**

```go
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

	got := r.snapshot(base)
	if len(got) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (10s window)", len(got))
	}
	// A second snapshot reflects pruning of the stale entry.
	if again := r.snapshot(base); len(again) != 2 {
		t.Fatalf("second snapshot len = %d, want 2", len(again))
	}
}

func TestSampleRing_HardCapEvictsOldest(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	r := newSampleRing(time.Hour, 3) // cap 3, generous window
	for i := 0; i < 5; i++ {
		r.add(sample{StatusCode: i}, base.Add(time.Duration(i)*time.Millisecond))
	}
	got := r.snapshot(base.Add(time.Second))
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (hard cap)", len(got))
	}
	if got[0].StatusCode != 2 {
		t.Fatalf("oldest retained StatusCode = %d, want 2 (0,1 evicted)", got[0].StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestSampleRing -v`
Expected: FAIL — `undefined: newSampleRing`.

- [ ] **Step 3: Write the implementation**

```go
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
// snapshot returns the trailing-window slice and prunes anything older; a hard
// length cap bounds memory under high RPS even within the window.
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
	return &sampleRing{window: window, maxLen: maxLen}
}

func (r *sampleRing) add(s sample, at time.Time) {
	r.mu.Lock()
	r.entries = append(r.entries, ringEntry{s: s, completedAt: at})
	if len(r.entries) > r.maxLen {
		r.entries = r.entries[len(r.entries)-r.maxLen:]
	}
	r.mu.Unlock()
}

func (r *sampleRing) snapshot(now time.Time) []sample {
	cutoff := now.Add(-r.window)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Prune entries older than the window (entries are append-ordered by time).
	keep := 0
	for keep < len(r.entries) && r.entries[keep].completedAt.Before(cutoff) {
		keep++
	}
	if keep > 0 {
		r.entries = r.entries[keep:]
	}
	out := make([]sample, len(r.entries))
	for i := range r.entries {
		out[i] = r.entries[i].s
	}
	return out
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestSampleRing -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/ring.go continuous-vllm-server-evaluator/ring_test.go
git commit -m "feat(continuous-eval): time-bounded trailing-window sample ring"
```

---

### Task 4: Scrape ring + trailing `/metrics` delta

Maintain a short history of timestamped `/metrics` scrapes so queue/inference-time means can be taken over the same trailing window as the client-side samples, rather than over a single inter-call interval.

**Files:**
- Create `continuous-vllm-server-evaluator/scrapering.go`
- Test `continuous-vllm-server-evaluator/scrapering_test.go`

**Interfaces:**
- Consumes: `metricsScrape` (Task 1), `windowDelta` (Task 1).
- Produces: `type scrapeRing`; `newScrapeRing(maxLen int) *scrapeRing`; `(*scrapeRing) add(m metricsScrape, at time.Time)`; `(*scrapeRing) trailingMeans(now time.Time, window time.Duration) (queueMeanSec, inferMeanSec float64)`. Means are per-completion seconds over the window; `0` when fewer than two scrapes or no completions occurred in the window.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"
)

func TestScrapeRing_TrailingMeansOverWindow(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	r := newScrapeRing(100)
	// oldest scrape (within window): sum=1.0 count=10
	r.add(metricsScrape{QueueTimeSum: 1.0, QueueTimeCount: 10, InferTimeSum: 5.0, InferTimeCount: 10}, base.Add(-10*time.Second))
	// latest: sum=1.6 count=16  → ΔQ=0.6 over ΔN=6 → 0.1s/req
	r.add(metricsScrape{QueueTimeSum: 1.6, QueueTimeCount: 16, InferTimeSum: 8.0, InferTimeCount: 16}, base)

	q, inf := r.trailingMeans(base, 30*time.Second)
	if q < 0.0999 || q > 0.1001 {
		t.Fatalf("queueMean = %v, want ~0.1", q)
	}
	if inf < 0.4999 || inf > 0.5001 { // ΔInfer=3.0 / ΔN=6 = 0.5
		t.Fatalf("inferMean = %v, want ~0.5", inf)
	}
}

func TestScrapeRing_SingleScrapeReturnsZero(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	r := newScrapeRing(100)
	r.add(metricsScrape{QueueTimeSum: 1.0, QueueTimeCount: 10}, base)
	q, inf := r.trailingMeans(base, 30*time.Second)
	if q != 0 || inf != 0 {
		t.Fatalf("means = (%v,%v), want (0,0) with a single scrape", q, inf)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestScrapeRing -v`
Expected: FAIL — `undefined: newScrapeRing`.

- [ ] **Step 3: Write the implementation**

```go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestScrapeRing -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/scrapering.go continuous-vllm-server-evaluator/scrapering_test.go
git commit -m "feat(continuous-eval): scrape ring + trailing /metrics delta over window"
```

---

### Task 5: Trailing aggregation

Fold a trailing-window sample slice (+ queue mean) into `AnalysisData`. Mirrors the existing `aggregate` math (means in ms, `Throughput ≤ RPS`) but computes throughput as `completed / windowSec` rather than over a closed window.

**Files:**
- Create `continuous-vllm-server-evaluator/aggregate.go`
- Test `continuous-vllm-server-evaluator/aggregate_test.go`

**Interfaces:**
- Consumes: `sample` (Task 1), `evaluator.AnalysisData`.
- Produces: `aggregateTrailing(samples []sample, pdRPS float32, windowSec float64, queueMeanSec float64) evaluator.AnalysisData`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func TestAggregateTrailing_Means(t *testing.T) {
	samples := []sample{
		{TTFT: 100 * time.Millisecond, ITLs: []time.Duration{20 * time.Millisecond, 30 * time.Millisecond}, ResponseTime: 200 * time.Millisecond},
		{TTFT: 120 * time.Millisecond, ITLs: []time.Duration{40 * time.Millisecond, 60 * time.Millisecond}, ResponseTime: 240 * time.Millisecond},
	}
	ad := aggregateTrailing(samples, 5.0 /*pdRPS*/, 10.0 /*windowSec*/, 0.1 /*queueMeanSec*/)

	if ad.AvgTTFT < 109 || ad.AvgTTFT > 111 { // (100+120)/2
		t.Errorf("AvgTTFT = %v, want ~110", ad.AvgTTFT)
	}
	if ad.AvgITL < 36 || ad.AvgITL > 39 { // mean of per-req ITL means: (25+50)/2=37.5
		t.Errorf("AvgITL = %v, want ~37.5", ad.AvgITL)
	}
	if ad.AvgWaitTime < 99 || ad.AvgWaitTime > 101 { // 0.1s -> 100ms
		t.Errorf("AvgWaitTime = %v, want ~100", ad.AvgWaitTime)
	}
	// throughput = 2 completed / 10s = 0.2, below RPS cap (5) → 0.2
	if ad.Throughput < 0.19 || ad.Throughput > 0.21 {
		t.Errorf("Throughput = %v, want ~0.2", ad.Throughput)
	}
}

func TestAggregateTrailing_ThroughputCappedAtRPS(t *testing.T) {
	samples := make([]sample, 100)
	for i := range samples {
		samples[i] = sample{TTFT: 10 * time.Millisecond, ResponseTime: 10 * time.Millisecond}
	}
	// 100 completed / 1s = 100, but RPS cap = 5
	ad := aggregateTrailing(samples, 5.0, 1.0, 0)
	if ad.Throughput != 5.0 {
		t.Errorf("Throughput = %v, want 5.0 (capped at RPS)", ad.Throughput)
	}
}

func TestAggregateTrailing_Empty(t *testing.T) {
	ad := aggregateTrailing(nil, 5.0, 10.0, 0)
	if ad != (evaluator.AnalysisData{}) {
		t.Errorf("empty samples → zero AnalysisData, got %+v", ad)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestAggregateTrailing -v`
Expected: FAIL — `undefined: aggregateTrailing`.

- [ ] **Step 3: Write the implementation**

```go
package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// aggregateTrailing folds the trailing-window samples (+ a queue-time mean from
// the scrape ring) into AnalysisData. Means are in ms; throughput is completed
// requests over the trailing window width, capped at the offered RPS.
func aggregateTrailing(samples []sample, pdRPS float32, windowSec float64, queueMeanSec float64) evaluator.AnalysisData {
	if len(samples) == 0 {
		return evaluator.AnalysisData{}
	}
	var ttftSum, rtSum, itlMeanSum float64
	var itlMeanCount, completed int
	for _, s := range samples {
		if s.Failed {
			continue
		}
		completed++
		ttftSum += float64(s.TTFT.Microseconds()) / 1000.0
		rtSum += float64(s.ResponseTime.Microseconds()) / 1000.0
		if len(s.ITLs) > 0 {
			var itlSum float64
			for _, itl := range s.ITLs {
				itlSum += float64(itl.Microseconds()) / 1000.0
			}
			itlMeanSum += itlSum / float64(len(s.ITLs))
			itlMeanCount++
		}
	}
	if completed == 0 {
		return evaluator.AnalysisData{}
	}
	avgITL := 0.0
	if itlMeanCount > 0 {
		avgITL = itlMeanSum / float64(itlMeanCount)
	}
	throughput := 0.0
	if windowSec > 0 {
		throughput = float64(completed) / windowSec
	}
	if float32(throughput) > pdRPS {
		throughput = float64(pdRPS)
	}
	return evaluator.AnalysisData{
		Throughput:  float32(throughput),
		AvgRespTime: float32(rtSum / float64(completed)),
		AvgWaitTime: float32(queueMeanSec * 1000.0),
		AvgTTFT:     float32(ttftSum / float64(completed)),
		AvgITL:      float32(avgITL),
		MaxRPS:      0, // vllm-server does not compute MaxRPS (pass-through policy)
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestAggregateTrailing -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/aggregate.go continuous-vllm-server-evaluator/aggregate_test.go
git commit -m "feat(continuous-eval): trailing-window aggregation to AnalysisData"
```

---

### Task 6: Trailing saturation detection

Adapt the existing overload signals (TTFT growth, error rate, queue dominance) to operate on a trailing-window sample slice plus the queue/inference means.

**Files:**
- Create `continuous-vllm-server-evaluator/saturation.go` (copy `errorRate`, `ttftGrowth`, `maxInt` from `vllm-server-evaluator/saturation.go`, then add the trailing entry point).
- Test `continuous-vllm-server-evaluator/saturation_test.go`

**Interfaces:**
- Consumes: `sample` (Task 1), `evaluator.SaturationOverload`/`SaturationNone`.
- Produces: `detectSaturationTrailing(samples []sample, minSamples int, queueMeanSec, inferMeanSec float64) string`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func TestDetectSaturationTrailing_HighErrorRate(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		s := sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond}
		if i < 3 { // 15% failures ≥ 5% threshold
			s.Failed = true
		}
		samples = append(samples, s)
	}
	if got := detectSaturationTrailing(samples, 5, 0.01, 0.05); got != evaluator.SaturationOverload {
		t.Errorf("got %q, want overload (high error rate)", got)
	}
}

func TestDetectSaturationTrailing_QueueDominates(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		samples = append(samples, sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond})
	}
	// queue mean (0.5s) dominates inference mean (0.05s) → overload
	if got := detectSaturationTrailing(samples, 5, 0.5, 0.05); got != evaluator.SaturationOverload {
		t.Errorf("got %q, want overload (queue dominance)", got)
	}
}

func TestDetectSaturationTrailing_Healthy(t *testing.T) {
	var samples []sample
	for i := 0; i < 20; i++ {
		samples = append(samples, sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond})
	}
	if got := detectSaturationTrailing(samples, 5, 0.01, 0.05); got != evaluator.SaturationNone {
		t.Errorf("got %q, want none (healthy)", got)
	}
}

func TestDetectSaturationTrailing_InsufficientSamples(t *testing.T) {
	samples := []sample{{TTFT: time.Second, Failed: true}}
	if got := detectSaturationTrailing(samples, 5, 1.0, 0.01); got != evaluator.SaturationNone {
		t.Errorf("got %q, want none (below minSamples, do not flag)", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestDetectSaturationTrailing -v`
Expected: FAIL — `undefined: detectSaturationTrailing`.

- [ ] **Step 3: Write the implementation**

Copy `errorRate`, `ttftGrowth`, and `maxInt` verbatim from `vllm-server-evaluator/saturation.go`, then add:

```go
// detectSaturationTrailing flags overload from the trailing-window samples plus
// the queue/inference means. Mirrors the windowed detector's three signals:
// TTFT growth across the window, error rate, and queue-time dominance. Returns
// SaturationNone when fewer than minSamples completed (cannot judge).
func detectSaturationTrailing(samples []sample, minSamples int, queueMeanSec, inferMeanSec float64) string {
	completed := 0
	for _, s := range samples {
		if !s.Failed {
			completed++
		}
	}
	if completed < minSamples {
		return evaluator.SaturationNone
	}
	if errorRate(samples) >= 0.05 {
		return evaluator.SaturationOverload
	}
	if ttftGrowth(samples) > 0.50 {
		return evaluator.SaturationOverload
	}
	// Queue dominance: time spent queueing far exceeds time spent inferring.
	if inferMeanSec > 0 && queueMeanSec > 2.0*inferMeanSec {
		return evaluator.SaturationOverload
	}
	return evaluator.SaturationNone
}
```

> Note: confirm the exact thresholds/predicates in the copied `errorRate`/`ttftGrowth` match the source. If `ttftGrowth`'s contract differs (e.g. requires ordered samples), keep the copied helper unchanged and only adapt the entry point.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestDetectSaturationTrailing -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/saturation.go continuous-vllm-server-evaluator/saturation_test.go
git commit -m "feat(continuous-eval): trailing-window saturation detection"
```

---

### Task 7: Generator — live config holder + persistent arrival loop

The heart of the binary: a `generator` holding the swappable live config, the limiter, the rings, and an **injectable** request function. `runLoop` issues Poisson arrivals until ctx is cancelled.

**Files:**
- Create `continuous-vllm-server-evaluator/generator.go`
- Test `continuous-vllm-server-evaluator/generator_test.go`

**Interfaces:**
- Consumes: `limiter`, `sampleRing`, `scrapeRing`, `tokenSampler`, `requestSpec`, `sample`, `pairingState`, `runOneRequest`.
- Produces:
  - `type liveConfig struct { rps float64; concurrency int; inSampler, outSampler tokenSampler; ignoreEOS bool; servedModel, queueMetric string; minSamples int; windowSec float64 }`
  - `type generator struct { ... }` with fields: `live atomic.Pointer[liveConfig]`, `pairing *atomic.Pointer[pairingState]`, `lim *limiter`, `ring *sampleRing`, `scrapes *scrapeRing`, `lookup map[string]serverConfig`, `baseURLOverride string`, `warmupSec int`, and injectable funcs `runOne func(ctx context.Context, baseURL, model string, spec requestSpec, seed int64) sample` and `scrape func(ctx context.Context, url, queueMetric string) (metricsScrape, error)`.
  - `newGenerator(lookup map[string]serverConfig) *generator` (wires production `runOne=runOneRequest`, `scrape=scrapeMetrics`, fresh rings/limiter).
  - `(*generator) baseURL() string` (from `baseURLOverride` or pairing `VLLMPodIP:VLLMPort`; `""` if unresolved).
  - `(*generator) runLoop(ctx context.Context)`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"math/rand"
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

	got := g.ring.snapshot(time.Now())
	if len(got) == 0 {
		t.Fatal("expected the loop to record completed samples")
	}
	if maxObserved > 4 {
		t.Fatalf("peak inflight = %d, must not exceed limit 4", maxObserved)
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
	_ = rand.Int // keep math/rand import if unused elsewhere
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestRunLoop -v`
Expected: FAIL — `undefined: newGenerator`.

- [ ] **Step 3: Write the implementation**

```go
package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"
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

	baseURLOverride string // test hook; empty in production
	warmupSec       int    // one-time warmup after loop start; samples before it are dropped

	// Injectable for tests; wired to runOneRequest / scrapeMetrics in production.
	runOne func(ctx context.Context, baseURL, model string, spec requestSpec, seed int64) sample
	scrape func(ctx context.Context, url, queueMetric string) (metricsScrape, error)
}

func newGenerator(lookup map[string]serverConfig) *generator {
	return &generator{
		lim:     newLimiter(evaluator_DefaultMaxConcurrency()),
		ring:    newSampleRing(30*time.Second, 200_000),
		scrapes: newScrapeRing(256),
		lookup:  lookup,
		runOne:  runOneRequest,
		scrape:  scrapeMetrics,
	}
}

// small indirection so the default limiter size matches the shared package.
func evaluator_DefaultMaxConcurrency() int { return 256 }

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
	rng := rand.New(rand.NewSource(1)) // deterministic stream; varies by arrival
	var seed int64
	startedAt := time.Now()
	warmupEnd := startedAt.Add(time.Duration(g.warmupSec) * time.Second)

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
			if now.Before(warmupEnd) {
				return // drop warmup-phase samples
			}
			g.ring.add(s, now)
		}()
	}
}
```

> Note on the limiter default: `newGenerator` sizes the limiter at 256 to match `evaluator.DefaultMaxConcurrency`; `/solve` (Task 8) calls `setLimit` with the resolved per-request concurrency before traffic matters. The `evaluator_DefaultMaxConcurrency` helper avoids importing the constant solely for a literal; if you prefer, import `evaluator.DefaultMaxConcurrency` directly and delete the helper.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run TestRunLoop -race -v`
Expected: PASS (both tests). If `TestRunLoop_FillsRingAndRespectsLimiter` is flaky on a loaded machine, widen the timeout, not the limit assertion.

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/generator.go continuous-vllm-server-evaluator/generator_test.go
git commit -m "feat(continuous-eval): live config holder + persistent Poisson arrival loop"
```

---

### Task 8: `/solve` — reconfigure + read trailing window

The handler: parse `ProblemData`, look up the per-(accelerator,model) config, build samplers, swap the live config, resize the limiter, scrape `/metrics`, and aggregate the trailing window into `AnalysisData`. Returns 503 if unpaired/cold, 500 on insufficient trailing samples, 200 otherwise.

**Files:**
- Create `continuous-vllm-server-evaluator/handler.go`
- Test `continuous-vllm-server-evaluator/handler_test.go`

**Interfaces:**
- Consumes: `generator` (Task 7), `aggregateTrailing` (Task 5), `detectSaturationTrailing` (Task 6), `evaluator.ProblemData`/`AnalysisData`/`ResolveMaxConcurrency`, `newSampler`, `loadConfig` lookup.
- Produces: `(*generator) solve(ctx context.Context, pd evaluator.ProblemData) (evaluator.AnalysisData, int, error)` (the `int` is an HTTP status for the error path); `solveHandler(g *generator) gin.HandlerFunc`; `roundTokenAvg(name string, v float32) (int, error)` (copy from `vllm-server-evaluator/handler.go`).

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func newTestGen() *generator {
	g := newGenerator(map[string]serverConfig{
		"H100|m": {
			VLLMServedModelName: "m",
			MinSamples:          5,
			QueueTimeMetric:     "vllm:request_queue_time_seconds",
			TrailingWindowSec:   30,
			DefaultConcurrency:  64,
		},
	})
	g.baseURLOverride = "http://fake"
	g.scrape = func(ctx context.Context, url, q string) (metricsScrape, error) {
		return metricsScrape{}, nil // no queue time in the unit test
	}
	return g
}

func TestSolve_ReconfiguresLiveConfig(t *testing.T) {
	g := newTestGen()
	pd := evaluator.ProblemData{RPS: 12, MaxConcurrency: 32, AvgInputTokens: 16, AvgOutputTokens: 8, Accelerator: "H100", Model: "m"}
	// Pre-seed the ring so we clear MinSamples.
	for i := 0; i < 6; i++ {
		g.ring.add(sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond}, time.Now())
	}
	_, status, err := g.solve(context.Background(), pd)
	if err != nil || status != http.StatusOK {
		t.Fatalf("solve: status=%d err=%v", status, err)
	}
	cfg := g.live.Load()
	if cfg == nil || cfg.rps != 12 || cfg.concurrency != 32 {
		t.Fatalf("live config not swapped: %+v", cfg)
	}
	if g.lim.currentLimit() != 32 {
		t.Fatalf("limiter limit = %d, want 32", g.lim.currentLimit())
	}
}

func TestSolve_InsufficientSamplesReturns500(t *testing.T) {
	g := newTestGen()
	pd := evaluator.ProblemData{RPS: 12, AvgInputTokens: 16, AvgOutputTokens: 8, Accelerator: "H100", Model: "m"}
	_, status, err := g.solve(context.Background(), pd) // ring empty
	if status != http.StatusInternalServerError || err == nil {
		t.Fatalf("want 500 + error on empty ring, got status=%d err=%v", status, err)
	}
}

func TestSolveHandler_HTTP(t *testing.T) {
	g := newTestGen()
	for i := 0; i < 6; i++ {
		g.ring.add(sample{TTFT: 50 * time.Millisecond, ITLs: []time.Duration{10 * time.Millisecond}, ResponseTime: 60 * time.Millisecond}, time.Now())
	}
	r := gin.New()
	r.POST("/solve", solveHandler(g))
	pd := evaluator.ProblemData{RPS: 12, AvgInputTokens: 16, AvgOutputTokens: 8, Accelerator: "H100", Model: "m"}
	body, _ := json.Marshal(pd)
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ad evaluator.AnalysisData
	if err := json.Unmarshal(rr.Body.Bytes(), &ad); err != nil {
		t.Fatal(err)
	}
	if ad.AvgTTFT == 0 {
		t.Errorf("expected non-zero AvgTTFT, got %+v", ad)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./continuous-vllm-server-evaluator/ -run 'TestSolve' -v`
Expected: FAIL — `undefined: solveHandler` / `g.solve undefined`.

- [ ] **Step 3: Write the implementation**

Copy `roundTokenAvg` verbatim from `vllm-server-evaluator/handler.go`, then add:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// solve reconfigures the live arrival loop from pd and returns metrics over the
// trailing window. Status is an HTTP code for the caller; 200 on success.
func (g *generator) solve(ctx context.Context, pd evaluator.ProblemData) (evaluator.AnalysisData, int, error) {
	sc, ok := g.lookup[pd.Accelerator+"|"+pd.Model]
	if !ok {
		return evaluator.AnalysisData{}, http.StatusBadRequest,
			fmt.Errorf("no config for %s|%s", pd.Accelerator, pd.Model)
	}
	base := g.baseURL()
	if base == "" {
		return evaluator.AnalysisData{}, http.StatusServiceUnavailable,
			fmt.Errorf("paired vLLM not resolved yet")
	}

	inAvg, err := roundTokenAvg("avgInputTokens", pd.AvgInputTokens)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	outAvg, err := roundTokenAvg("avgOutputTokens", pd.AvgOutputTokens)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	inSampler, err := newSampler(sc.InputTokenDistribution, inAvg)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	outSampler, err := newSampler(sc.OutputTokenDistribution, outAvg)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	conc := evaluator.ResolveMaxConcurrency(pd.MaxConcurrency, sc.DefaultConcurrency, "continuous-vllm-server")
	windowSec := float64(sc.TrailingWindowSec)

	// Swap the live config and resize the limiter — the running loop picks these
	// up on its next arrival. This is the "reconfigure, keep running" step.
	g.live.Store(&liveConfig{
		rps:         float64(pd.RPS),
		concurrency: conc,
		inSampler:   inSampler,
		outSampler:  outSampler,
		ignoreEOS:   sc.IgnoreEOS,
		servedModel: sc.VLLMServedModelName,
		queueMetric: sc.QueueTimeMetric,
		minSamples:  sc.MinSamples,
		windowSec:   windowSec,
	})
	g.lim.setLimit(conc)

	// Scrape /metrics now and record it; queue/inference means come from the
	// trailing delta across the scrape ring.
	now := time.Now()
	if m, serr := g.scrape(ctx, base+"/metrics", sc.QueueTimeMetric); serr == nil {
		g.scrapes.add(m, now)
	}
	queueMeanSec, inferMeanSec := g.scrapes.trailingMeans(now, time.Duration(windowSec)*time.Second)

	samples := g.ring.snapshot(now)
	completed := 0
	for _, s := range samples {
		if !s.Failed {
			completed++
		}
	}
	if completed < sc.MinSamples {
		return evaluator.AnalysisData{}, http.StatusInternalServerError,
			fmt.Errorf("insufficient samples: need %d, got %d", sc.MinSamples, completed)
	}

	ad := aggregateTrailing(samples, pd.RPS, windowSec, queueMeanSec)
	ad.Saturation = detectSaturationTrailing(samples, sc.MinSamples, queueMeanSec, inferMeanSec)
	return ad, http.StatusOK, nil
}

func solveHandler(g *generator) gin.HandlerFunc {
	return func(c *gin.Context) {
		var pd evaluator.ProblemData
		if err := c.ShouldBindJSON(&pd); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad request: " + err.Error()})
			return
		}
		ad, status, err := g.solve(c.Request.Context(), pd)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, ad)
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./continuous-vllm-server-evaluator/ -run 'TestSolve' -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add continuous-vllm-server-evaluator/handler.go continuous-vllm-server-evaluator/handler_test.go
git commit -m "feat(continuous-eval): /solve reconfigure + trailing-window read"
```

---

### Task 9: `main.go` wiring + build integration

Wire the binary: load config, start the pairing resolver, launch `runLoop`, serve `POST /solve`. Add the build line and the `evaluator.sh` case so the container can select this backend.

**Files:**
- Create `continuous-vllm-server-evaluator/main.go`
- Modify `Dockerfile.evaluator` (add one build line + one COPY line)
- Modify `evaluator.sh` (add one case)
- Create `continuous-vllm-server-evaluator/vllm-eval-config.example.json` (sample with `trailingWindowSec`)

**Interfaces:**
- Consumes: `newGenerator`, `loadConfig`, `resolvePairing`, `solveHandler`, `(*generator).runLoop`.

- [ ] **Step 1: Write `main.go`** (model on `vllm-server-evaluator/main.go`; reuse its env handling)

```go
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	lookup, err := loadConfig()
	if err != nil {
		log.Fatalf("loadConfig: %v", err)
	}
	log.Printf("continuous-vllm-server-evaluator: loaded %d config entries", len(lookup))

	port := 8081
	if v := os.Getenv("EVALUATOR_PORT"); v != "" {
		if p, perr := strconv.Atoi(v); perr == nil {
			port = p
		}
	}

	// Determine the vLLM port (all entries share one, default 8000).
	vllmPort := 8000
	for _, sc := range lookup {
		if sc.VLLMPort > 0 {
			vllmPort = sc.VLLMPort
			break
		}
	}

	g := newGenerator(lookup)
	// Warmup applies once at loop start; reuse the first entry's WarmupSec if set.
	for _, sc := range lookup {
		g.warmupSec = sc.WarmupSec
		break
	}

	// Background pairing resolver (same cadence as the windowed binary).
	if cfg, cerr := rest.InClusterConfig(); cerr == nil {
		if client, kerr := kubernetes.NewForConfig(cfg); kerr == nil {
			go func() {
				for {
					rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					ps, rerr := resolvePairing(rctx, client, vllmPort)
					cancel()
					if rerr == nil {
						g.pairing.Store(ps)
					} else {
						log.Printf("pairing: %v", rerr)
					}
					time.Sleep(15 * time.Second)
				}
			}()
		} else {
			log.Printf("k8s client: %v (pairing disabled)", kerr)
		}
	} else {
		log.Printf("not in cluster: %v (pairing disabled)", cerr)
	}

	// Persistent arrival loop.
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.runLoop(loopCtx)

	r := gin.Default()
	r.POST("/solve", solveHandler(g))
	log.Printf("continuous-vllm-server-evaluator listening on :%d", port)
	if err := r.Run(":" + strconv.Itoa(port)); err != nil {
		log.Fatalf("server: %v", err)
	}
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./continuous-vllm-server-evaluator/`
Expected: builds clean.

- [ ] **Step 3: Add the build + COPY lines to `Dockerfile.evaluator`**

In the builder stage, after the `vllm-server-evaluator` build line:
```dockerfile
RUN go build -o continuous-vllm-server-evaluator ./continuous-vllm-server-evaluator
```
In the final stage, after the `vllm-server-evaluator` COPY line:
```dockerfile
COPY --from=builder /app/continuous-vllm-server-evaluator .
```

- [ ] **Step 4: Add the case to `evaluator.sh`**

```sh
  continuous-vllm-server) exec ./continuous-vllm-server-evaluator ;;
```
(Place it before the `*)` default; add `continuous-vllm-server` to the usage hint string.)

- [ ] **Step 5: Create the example config** `continuous-vllm-server-evaluator/vllm-eval-config.example.json`

```json
{
  "configs": [
    {
      "accelerator": "H100",
      "model": "llama_3_1_8b",
      "vllmServedModelName": "unsloth/Meta-Llama-3.1-8B-Instruct",
      "vllmPort": 8000,
      "warmupSec": 10,
      "minSamples": 20,
      "ignoreEOS": true,
      "queueTimeMetric": "vllm:request_queue_time_seconds",
      "inputTokenDistribution": "uniform-bounded",
      "outputTokenDistribution": "uniform-bounded",
      "defaultConcurrency": 128,
      "trailingWindowSec": 30
    }
  ]
}
```

- [ ] **Step 6: Full package test + vet**

Run: `go vet ./continuous-vllm-server-evaluator/ && go test ./continuous-vllm-server-evaluator/ -race`
Expected: PASS, no vet complaints.

- [ ] **Step 7: Commit**

```bash
git add continuous-vllm-server-evaluator/main.go continuous-vllm-server-evaluator/vllm-eval-config.example.json Dockerfile.evaluator evaluator.sh
git commit -m "feat(continuous-eval): main wiring + Dockerfile/evaluator.sh backend selection"
```

---

## Out of scope (follow-ups, not this plan)

- **server-sim loop cleanup.** Removing `watchAllocation` abandon-and-restart (`pkg/server/loop.go`) is *optional* — it is a harmless no-op against a fast `/solve`. Leave server-sim untouched so the A/B baseline arm is unchanged; remove it in a later cleanup PR once continuous is adopted.
- **control-loop A/B harness.** Deploy manifests selecting `continuous-vllm-server` vs `vllm-server` as the two arms, plus the experiment report under `experiments/`, live in control-loop (mirrors the 2026-06-18 split). Tracked separately.
- **Generation tagging (option c).** The escape hatch if the EKF destabilizes on transients (design doc §"Consequence: causal gating is traded away"). Not built unless the A/B shows it is needed.

## Self-review notes

- **Spec coverage:** persistent loop (T7), atomic config swap (T8), resizable limiter (T2), ring-buffer trailing stats (T3+T5), `/metrics` deltas across scrapes (T4+T8), warmup-collapses-to-one-time (T7 `warmupEnd`), pass-through saturation (T6), drop-if-full parity (T2+T7), `Throughput ≤ RPS` (T5), new-binary-via-deploy-selection (T9). All design-doc mechanical items are covered.
- **Type consistency:** `liveConfig`, `generator`, `sampleRing`, `scrapeRing`, `limiter`, `aggregateTrailing`, `detectSaturationTrailing`, `solve`, `solveHandler` names are used identically across tasks.
- **Open implementation detail to confirm during T6:** the exact thresholds in the copied `ttftGrowth`/`errorRate` — keep the copied helpers verbatim and only adapt the entry point predicate, then tune the test expectations to the real thresholds if they differ from the 5%/50% used here.
