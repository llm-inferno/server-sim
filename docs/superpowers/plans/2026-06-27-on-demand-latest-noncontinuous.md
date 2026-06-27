# On-Demand `/latest` for Non-Continuous Backends — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `GET /latest` compute a result synchronously from the current in-force labels when `SERVERSIM_CONTINUOUS=false`, so simulator backends (`blis`, `queue-analysis`) get a fresh, allocation-coherent estimate every cycle instead of a permanent `404`.

**Architecture:** server-sim decides what `/latest` *means* from its own mode. Continuous → serve the most-recent loop-completed window (lookback, unchanged). Non-continuous → read the downward-API labels, solve once via the existing `solveWithPolicy`, return the envelope. The collector, the coherence gate, and `pkg/evaluator` are untouched.

**Tech Stack:** Go, gin, server-sim `pkg/server`. Tests with the standard library + `httptest`.

**Spec:** `docs/superpowers/specs/2026-06-27-on-demand-latest-noncontinuous-design.md`
**Architecture notes:** `docs/serversim-latest-architecture.md`

## Global Constraints

- Module: `github.com/llm-inferno/server-sim`. No new third-party dependencies.
- All code `gofmt`-clean. `go vet ./...` clean.
- `go test -race ./pkg/server/ ./pkg/job/` must stay green at every commit.
- Follow existing `pkg/server` patterns; do **not** touch `watchAllocation` / `alloc_watch.go` (it serves the windowed `vllm-server` loop; independent of this work — see spec §7).
- Continuous-mode behaviour (real-vLLM backends) must remain byte-for-byte unchanged.
- Branch: `feat/on-demand-latest-noncontinuous` (already created off `main`, spec + arch-notes already committed).

---

### Task 1: Factor the shared solve core out of `runOnce`

Extract two small helpers so the on-demand `/latest` path and the loop share the label-read and solve+offered-load logic, **without disturbing `watchAllocation`** (the loop still reads `pd.MaxConcurrency` itself before starting the watcher).

**Files:**
- Modify: `pkg/server/loop.go`
- Test: `pkg/server/loop_test.go`

**Interfaces:**
- Consumes: `ReadLabels`, `LabelsToProblemData` (`pkg/server/labels.go`); `solveWithPolicy` (`pkg/server/policy.go`); `solver` interface (`pkg/server/policy.go:18`); existing test helper `okSolver` (`pkg/server/loop_test.go`).
- Produces:
  - `func readProblem(labelsPath string) (evaluator.ProblemData, bool)` — `ok=false` when the pod is not ready (labels missing/unparseable).
  - `func solveCurrent(ctx context.Context, cli solver, policy string, pd evaluator.ProblemData) (evaluator.ProblemData, evaluator.AnalysisData, error)` — `solveWithPolicy` plus the `OfferedRPS` substitution.

- [ ] **Step 1: Write failing tests for `solveCurrent`**

Add to `pkg/server/loop_test.go`:

```go
func TestSolveCurrentSubstitutesOfferedRPS(t *testing.T) {
	pd := evaluator.ProblemData{RPS: 5, MaxConcurrency: 32}
	cli := okSolver{ad: evaluator.AnalysisData{OfferedRPS: 9, Throughput: 4}}
	eff, ad, err := solveCurrent(context.Background(), cli, config.SaturationPolicyPassThrough, pd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if eff.RPS != 9 {
		t.Fatalf("eff.RPS = %v, want 9 (OfferedRPS substitution)", eff.RPS)
	}
	if ad.Throughput != 4 {
		t.Fatalf("ad.Throughput = %v, want 4", ad.Throughput)
	}
}

func TestSolveCurrentKeepsRPSWhenNoOffered(t *testing.T) {
	pd := evaluator.ProblemData{RPS: 5, MaxConcurrency: 32}
	cli := okSolver{ad: evaluator.AnalysisData{Throughput: 4}} // OfferedRPS = 0
	eff, _, err := solveCurrent(context.Background(), cli, config.SaturationPolicyPassThrough, pd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if eff.RPS != 5 {
		t.Fatalf("eff.RPS = %v, want 5 (unchanged)", eff.RPS)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd ../server-sim && go test ./pkg/server/ -run TestSolveCurrent -v`
Expected: FAIL — `undefined: solveCurrent`.

- [ ] **Step 3: Add the two helpers and rewire `runOnce`**

In `pkg/server/loop.go`, add `"github.com/llm-inferno/server-sim/pkg/evaluator"` to the imports, then add the helpers and replace `runOnce`'s body:

```go
// readProblem reads the current downward-API labels and converts them to a
// ProblemData. ok=false when the pod is not yet ready (labels missing or
// unparseable) — the loop skips, the on-demand /latest path returns 404.
func readProblem(labelsPath string) (evaluator.ProblemData, bool) {
	labels, err := ReadLabels(labelsPath)
	if err != nil {
		return evaluator.ProblemData{}, false
	}
	return LabelsToProblemData(labels)
}

// solveCurrent solves pd under the saturation policy and applies the
// window-averaged offered-load substitution. Shared by the continuous loop and
// the on-demand /latest path so both produce identical envelopes. When the
// evaluator measured a window-averaged offered load (continuous-vllm-server),
// it is reported as the effective offered rate; other backends leave OfferedRPS
// at 0 and the setpoint (or retry-reduced) RPS is preserved.
func solveCurrent(ctx context.Context, cli solver, policy string, pd evaluator.ProblemData) (evaluator.ProblemData, evaluator.AnalysisData, error) {
	eff, ad, err := solveWithPolicy(ctx, cli, policy, pd)
	if err != nil {
		return eff, ad, err
	}
	if ad.OfferedRPS > 0 {
		eff.RPS = ad.OfferedRPS
	}
	return eff, ad, nil
}
```

Replace the existing `runOnce` with:

```go
// runOnce executes a single window. It reads the current labels, creates a job,
// runs the window (cancelling it if the allocation concurrency changes
// mid-flight), and stores the effective input + result. Silently skips when the
// pod is not yet ready or the window fails — the Collector handles the
// resulting absence/staleness.
func (l *Loop) runOnce(parent context.Context) {
	pd, ok := readProblem(l.labelsPath)
	if !ok {
		return // not ready
	}

	id := l.jobs.Create()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Watch for an allocation change; cancel the in-flight window so the next
	// window runs under the new concurrency promptly.
	startConc := pd.MaxConcurrency
	go l.watchAllocation(ctx, cancel, startConc)

	eff, result, err := solveCurrent(ctx, l.cli, l.cfg.SaturationPolicy, pd)
	if err != nil {
		l.jobs.Fail(id, err.Error())
		// A cancelled window is the watcher abandoning it on an allocation
		// change, not a solver failure — distinguish the two in the log.
		if errors.Is(err, context.Canceled) {
			log.Printf("loop: window abandoned (allocation changed mid-flight): %v", err)
		} else {
			log.Printf("loop: window failed (skipping publish): %v", err)
		}
		return
	}
	l.jobs.Complete(id, eff, result)
}
```

- [ ] **Step 4: Run the full server package test suite (race)**

Run: `cd ../server-sim && go test -race ./pkg/server/ -v`
Expected: PASS — the new `TestSolveCurrent*` tests pass, and **all existing loop tests still pass** (`TestRunOnceSkipsWhenLabelsMissing`, `TestRunOnceLogsAbandonedOnCancel`, the `okSolver`-driven success test), confirming the refactor is behaviour-preserving.

- [ ] **Step 5: Verify gofmt/vet**

Run: `cd ../server-sim && gofmt -l pkg/server/ && go vet ./pkg/server/`
Expected: no output (clean).

- [ ] **Step 6: Commit**

```bash
cd ../server-sim
git add pkg/server/loop.go pkg/server/loop_test.go
git commit -m "refactor(server): extract readProblem + solveCurrent from runOnce

Shared label-read and solve+offered-load helpers, prep for the on-demand
/latest path. Behaviour-preserving; watchAllocation untouched.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: On-demand `/latest` for non-continuous mode

Add the mode branch to `handleLatest`: continuous → existing lookback; non-continuous → compute on demand from the labels via `computeLatest`.

**Files:**
- Modify: `pkg/server/server.go` (add `labelsPath` field, set it in `New`, branch `handleLatest`)
- Modify: `pkg/server/loop.go` (add `computeLatest`)
- Test: `pkg/server/server_test.go`

**Interfaces:**
- Consumes: `readProblem`, `solveCurrent` (Task 1); `Server.cfg`, `Server.evalCli`, `Server.jobs` (`pkg/server/server.go`); test helpers `mockEvaluator`, `writeLabels`, `sampleLabels` (existing, same package). `sampleLabels` carries `maxbatchsize="32"`, `rpm="300"`, `qwen_2_5_14b` / `H100`.
- Produces: `func computeLatest(ctx context.Context, policy string, cli solver, labelsPath string) (evaluator.ProblemData, evaluator.AnalysisData, bool, error)` — `ok=false` ⇒ not ready (404); `err != nil` ⇒ solve failure (500).

- [ ] **Step 1: Write failing tests for the on-demand path and update the two lookback tests**

In `pkg/server/server_test.go`, **replace** `TestLatestColdStart404` and `TestLatestReturnsEnvelope` with the continuous-mode versions below (the lookback path now requires `ContinuousMode: true`; `TickInterval: time.Minute` keeps the loop from ticking during the test, `defer s.Shutdown()` stops it):

```go
func TestLatestColdStart404(t *testing.T) {
	eval := mockEvaluator(t, evaluator.AnalysisData{AvgITL: 5})
	defer eval.Close()
	s := New(config.Config{EvaluatorURL: eval.URL, JobTTL: time.Minute, ContinuousMode: true, TickInterval: time.Minute})
	defer s.Shutdown()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/latest")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("continuous cold-start /latest = %d, want 404", resp.StatusCode)
	}
}

func TestLatestReturnsEnvelope(t *testing.T) {
	eval := mockEvaluator(t, evaluator.AnalysisData{AvgITL: 5, Throughput: 3})
	defer eval.Close()
	s := New(config.Config{EvaluatorURL: eval.URL, JobTTL: time.Minute, ContinuousMode: true, TickInterval: time.Minute})
	defer s.Shutdown()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	id := submitJob(t, srv) // populate the job store via POST /simulate
	pollJob(t, srv, id)

	resp, err := http.Get(srv.URL + "/latest")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/latest status err=%v code=%d", err, resp.StatusCode)
	}
	var env struct {
		EffectiveInput evaluator.ProblemData  `json:"effectiveInput"`
		Result         evaluator.AnalysisData `json:"result"`
		CompletedAt    string                 `json:"completedAt"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Result.AvgITL != 5 || env.CompletedAt == "" {
		t.Fatalf("bad envelope: %+v", env)
	}
}
```

Then **add** the new on-demand tests:

```go
func TestLatestOnDemandComputesFromLabels(t *testing.T) {
	eval := mockEvaluator(t, evaluator.AnalysisData{AvgITL: 7, Throughput: 4})
	defer eval.Close()
	dir := t.TempDir()
	writeLabels(t, dir) // sampleLabels: maxbatchsize=32, rpm=300, qwen_2_5_14b/H100
	s := New(config.Config{EvaluatorURL: eval.URL, JobTTL: time.Minute, LabelsDir: dir}) // ContinuousMode off
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/latest")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("on-demand /latest status err=%v code=%d", err, resp.StatusCode)
	}
	var env struct {
		EffectiveInput evaluator.ProblemData  `json:"effectiveInput"`
		Result         evaluator.AnalysisData `json:"result"`
		CompletedAt    string                 `json:"completedAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Coherence by construction: effectiveInput.concurrency == in-force maxbatchsize.
	if env.EffectiveInput.MaxConcurrency != 32 {
		t.Fatalf("effectiveInput.MaxConcurrency = %d, want 32 (in-force maxbatchsize)", env.EffectiveInput.MaxConcurrency)
	}
	if env.Result.AvgITL != 7 || env.CompletedAt == "" {
		t.Fatalf("bad on-demand envelope: %+v", env)
	}
}

func TestLatestOnDemandNotReady404(t *testing.T) {
	eval := mockEvaluator(t, evaluator.AnalysisData{AvgITL: 5})
	defer eval.Close()
	dir := t.TempDir() // no labels file written → pod not ready
	s := New(config.Config{EvaluatorURL: eval.URL, JobTTL: time.Minute, LabelsDir: dir})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/latest")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("on-demand /latest with no labels = %d, want 404", resp.StatusCode)
	}
}

func TestLatestOnDemandSolveError500(t *testing.T) {
	// Evaluator returns 500 → SolveCtx errors → /latest surfaces 500.
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer eval.Close()
	dir := t.TempDir()
	writeLabels(t, dir)
	s := New(config.Config{EvaluatorURL: eval.URL, JobTTL: time.Minute, LabelsDir: dir})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/latest")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("on-demand /latest with solver error = %d, want 500", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd ../server-sim && go test ./pkg/server/ -run TestLatest -v`
Expected: FAIL — `undefined: computeLatest`, and the on-demand tests fail because the current `handleLatest` ignores mode and reads the (empty) job store.

- [ ] **Step 3: Add `computeLatest` to `loop.go`**

```go
// computeLatest reads the current labels and solves on demand, returning the
// envelope fields for the non-continuous /latest path. ok=false ⇒ pod not ready
// (caller returns 404). err != nil ⇒ solve failure (caller returns 500).
func computeLatest(ctx context.Context, policy string, cli solver, labelsPath string) (evaluator.ProblemData, evaluator.AnalysisData, bool, error) {
	pd, ok := readProblem(labelsPath)
	if !ok {
		return evaluator.ProblemData{}, evaluator.AnalysisData{}, false, nil
	}
	eff, ad, err := solveCurrent(ctx, cli, policy, pd)
	if err != nil {
		return eff, ad, false, err
	}
	return eff, ad, true, nil
}
```

- [ ] **Step 4: Add `labelsPath` to `Server`, set it in `New`, and branch `handleLatest`**

In `pkg/server/server.go`:

1. Add `"path/filepath"` and `"time"` to the import block.
2. Add the field to the `Server` struct:

```go
type Server struct {
	router     *gin.Engine
	cfg        config.Config
	evalCli    *evaluator.Client
	jobs       *job.Manager
	labelsPath string             // downward-API labels file; used by on-demand /latest
	cancel     context.CancelFunc // cancels the continuous loop; nil when not running
}
```

3. Set it in `New` (add to the struct literal):

```go
	s := &Server{
		router:     gin.Default(),
		cfg:        cfg,
		evalCli:    evaluator.NewClient(cfg.EvaluatorURL),
		jobs:       job.NewManager(cfg.JobTTL),
		labelsPath: filepath.Join(cfg.LabelsDir, "labels"),
	}
```

4. Replace `handleLatest`:

```go
// handleLatest returns the current performance estimate as a self-describing
// envelope. In continuous mode it serves the most-recent loop-completed window
// (a lookback). In non-continuous mode it computes a fresh result on demand from
// the current in-force labels — so effectiveInput.concurrency always equals the
// in-force maxbatchsize and the collector's coherence gate passes by construction.
func (s *Server) handleLatest(c *gin.Context) {
	if s.cfg.ContinuousMode {
		j := s.jobs.Latest()
		if j == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no result yet"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"effectiveInput": j.EffectiveInput,
			"result":         j.Result,
			"completedAt":    j.CompletedAt,
		})
		return
	}

	// Non-continuous (simulator) backend: compute on demand against the current
	// labels. Thread the request context so the collector's GET /latest timeout
	// aborts a too-long solve cleanly rather than orphaning it.
	eff, ad, ok, err := computeLatest(c.Request.Context(), s.cfg.SaturationPolicy, s.evalCli, s.labelsPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no result yet"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"effectiveInput": eff,
		"result":         ad,
		"completedAt":    time.Now().UTC(),
	})
}
```

- [ ] **Step 5: Run the server package test suite (race)**

Run: `cd ../server-sim && go test -race ./pkg/server/ ./pkg/job/ -v`
Expected: PASS — all `TestLatest*` (lookback + on-demand), `TestSolveCurrent*`, the loop tests, and the `Shutdown` test pass.

- [ ] **Step 6: Verify gofmt/vet and the full build**

Run: `cd ../server-sim && gofmt -l pkg/server/ && go vet ./... && go build ./...`
Expected: no output from gofmt, clean vet, successful build.

- [ ] **Step 7: Commit**

```bash
cd ../server-sim
git add pkg/server/server.go pkg/server/loop.go pkg/server/server_test.go
git commit -m "feat(server): on-demand /latest for non-continuous backends

When SERVERSIM_CONTINUOUS=false, GET /latest computes a fresh result from
the current in-force labels (read -> solveWithPolicy -> envelope) instead
of serving the empty job store. effectiveInput.concurrency equals the
in-force maxbatchsize by construction, so the collector coherence gate
passes every cycle. Continuous (real-vLLM) lookback path unchanged.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Document the non-continuous `/latest` behaviour

**Files:**
- Modify: `CLAUDE.md` (server-sim repo)

**Interfaces:** none (docs).

- [ ] **Step 1: Update the Continuous-mode section**

In `CLAUDE.md`, immediately after the paragraph at line 46 ("The result is stored in the job store and served by `GET /latest`…"), add:

```markdown
When `SERVERSIM_CONTINUOUS=false` (the default), no background loop runs. `GET /latest` instead computes a result **on demand**: it reads the current downward-API labels, solves once via `solveWithPolicy` (same saturation policy as the loop), and returns the freshly-computed envelope. This suits the pure-function simulator backends (`queue-analysis`, `blis`), whose `/solve` is synchronous and deterministic. Because the envelope is built from labels read at request time, `effectiveInput.concurrency` always equals the in-force `maxbatchsize`, so the control-loop collector's causal-coherence gate passes by construction (no stale-window flip-flop). `404 {"error":"no result yet"}` is returned only while the labels file is not yet populated (pod not ready); there is no cold-start window to wait for.
```

- [ ] **Step 2: Update the `SERVERSIM_CONTINUOUS` env-var row**

Replace the table row at line 91:

```markdown
| `SERVERSIM_CONTINUOUS` | `false` | When `true`, run the background evaluation loop (windows from the labels file each tick) feeding `GET /latest`. When `false`, `GET /latest` computes on demand from the labels file. |
```

- [ ] **Step 3: Commit**

```bash
cd ../server-sim
git add CLAUDE.md
git commit -m "docs: document non-continuous on-demand /latest in CLAUDE.md

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Flip simulator deployments to non-continuous (control-loop repo)

This is the control-loop-side contract change (spec §6). It belongs in the **control-loop** repo and depends on the rebuilt server-sim image carrying Tasks 1–2. Do this on a control-loop branch, separate from the server-sim PR.

**Files (control-loop repo):**
- Modify: `manifests/blis/dep-blis-qwen.yaml` (run19 target)
- Modify: `manifests/blis/dep-blis-granite.yaml`, `manifests/blis/dep-blis-llama.yaml`
- Modify: `manifests/qa/dep-qa-granite.yaml`, `manifests/qa/dep-qa-llama.yaml`

**Interfaces:** none (manifests).

- [ ] **Step 1: Create a control-loop branch**

```bash
cd /Users/tantawi/Projects/llm-inferno/control-loop
git checkout main && git pull && git checkout -b feat/blis-qa-noncontinuous-latest
```

- [ ] **Step 2: Set `SERVERSIM_CONTINUOUS` to `"false"` in each manifest**

In each of the five files, change the `server-sim` container env value:

```yaml
        - name: SERVERSIM_CONTINUOUS
          value: "false"
```

(Currently `"true"` in all five — verify with `grep -n 'SERVERSIM_CONTINUOUS' -A1 manifests/blis/dep-*.yaml manifests/qa/dep-*.yaml`.)

- [ ] **Step 3: Verify the change**

Run: `grep -n 'SERVERSIM_CONTINUOUS' -A1 manifests/blis/dep-blis-qwen.yaml manifests/blis/dep-blis-granite.yaml manifests/blis/dep-blis-llama.yaml manifests/qa/dep-qa-granite.yaml manifests/qa/dep-qa-llama.yaml`
Expected: all five show `value: "false"`.

- [ ] **Step 4: Commit**

```bash
cd /Users/tantawi/Projects/llm-inferno/control-loop
git add manifests/blis/dep-blis-qwen.yaml manifests/blis/dep-blis-granite.yaml manifests/blis/dep-blis-llama.yaml manifests/qa/dep-qa-granite.yaml manifests/qa/dep-qa-llama.yaml
git commit -m "feat(blis,qa): run simulators in non-continuous on-demand /latest mode

Simulator backends compute /latest on demand (server-sim feat/on-demand-latest),
eliminating the M*-churn coherence-gate replica flip-flop. Requires the rebuilt
server-sim/evaluator image.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Workflow: issue, image rebuild, PRs

These wrap the code tasks; do them at the points noted.

- [ ] **Before Task 1 — open the server-sim issue.** `gh issue create` in the server-sim repo: title "On-demand /latest for non-continuous (simulator) backends"; body summarizing the bug (`SERVERSIM_CONTINUOUS=false` → permanent 404 because `/latest` only serves the loop-fed job store) and the fix (compute on demand), linking the spec path. Capture the issue number.
- [ ] **After Task 3 — open the server-sim PR** from `feat/on-demand-latest-noncontinuous` to `main`, "Closes #<issue>". Include the spec + arch-notes (already committed on the branch). Request review.
- [ ] **After the server-sim PR merges — rebuild the images** so the cluster picks up the fix (`Dockerfile.server-sim` and `Dockerfile.evaluator`), per the cross-arch build note in memory (`podman build --platform=linux/amd64`), and load/push as the deploy flow requires.
- [ ] **After the image is rebuilt — open the control-loop PR** for Task 4 (`feat/blis-qa-noncontinuous-latest` → `main`), noting it requires the rebuilt image. (The run19 experiment branch then rebases onto this; run19 execution itself is out of scope for this plan.)

---

## Self-Review

**Spec coverage:**
- §4.1 factor the solve core → Task 1 (`readProblem` + `solveCurrent`; `computeLatest` in Task 2). *Refinement vs. spec:* split into two helpers + `computeLatest` instead of one function, because `runOnce` needs `pd.MaxConcurrency` before solving to start `watchAllocation`; the loop reads labels itself and is otherwise unchanged. Same shared-core intent, no loop disturbance.
- §4.2 start loop only in continuous mode → already true in `New`; no change needed (noted).
- §4.3 on-demand `handleLatest` (mode branch, request-context cancellation, no caching, coherence by construction) → Task 2 Step 4.
- §4.4 envelope parity → Task 2 (`completedAt = time.Now().UTC()` on-demand; covered by `TestLatestOnDemandComputesFromLabels`).
- §5 behaviour matrix → enforced by the lookback (continuous) + on-demand tests in Task 2.
- §6 control-loop contract → Task 4.
- §7 #25 independence → Global Constraints (do not touch `watchAllocation`).
- §8 testing (coherence invariant, 404 not-ready, 500 solve error, continuous regression, `computeLatest` parity, `-race`) → Task 1 + Task 2 tests; `-race` in Task 2 Step 5.
- §9 risks → no code action required (documented); latency validated at cluster smoke (run19, out of scope).

**Placeholder scan:** none — all steps carry complete code and exact commands.

**Type consistency:** `readProblem`, `solveCurrent`, `computeLatest` signatures match across Tasks 1–2 and the `handleLatest` call site. `solver` interface satisfied by `*evaluator.Client` (`s.evalCli`) and by the test `okSolver`. `Server` field `labelsPath` set in `New` and read in `handleLatest`. `config.Config` fields used (`ContinuousMode`, `TickInterval`, `LabelsDir`, `SaturationPolicy`, `EvaluatorURL`, `JobTTL`) all exist.
