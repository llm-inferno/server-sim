# On-Demand `/latest` for Non-Continuous (Simulator) Backends — Design

**Status:** Draft for review
**Date:** 2026-06-27
**Scope:** server-sim repo only. No changes to control-loop, the collector, or `pkg/evaluator`. The control-loop side is a single manifest edit (`SERVERSIM_CONTINUOUS=false` on simulator deployments), specified here as the contract this change satisfies.

## 1. Motivation

The continuous background loop (`SERVERSIM_CONTINUOUS=true`, `pkg/server/loop.go`) was built for the **real-vLLM** backends, where `/solve` samples an actual running server over a wall-clock measurement window. For those backends there is no choice: you must observe the server's *past* behaviour over a window, so server-sim runs windows back-to-back and `GET /latest` serves the most-recent completed one.

The **simulator** backends — `queue-analysis` (analytical) and `blis` (DES) — are different in kind. Their `/solve` is a **pure, synchronous function** of `ProblemData`: given `(load, concurrency)` it computes and returns the predicted steady-state performance immediately, with no background state. Wrapping a pure function in a background ticker loop + job cache buys nothing and introduces a failure mode:

- The loop runs a window under the M\* that was in force when the window started.
- The control-loop Actuator then rewrites the `maxbatchsize` label (a new M\*).
- `GET /latest` still serves the *stale* window (old M\*), so the collector's causal-coherence gate (`effectiveInput.concurrency != inForce`) rejects **every** reporting pod that cycle.
- Deployment throughput collapses to 0; `arrivalRate` falls back to the emulator offered setpoint; the optimizer sees high-offered/zero-throughput and over-scales; it corrects the next cycle once a fresh window lands.

This is the **replica flip-flop / transient zero-throughput** observed in run19 (1→2→1→3→1→5→2 over ~7 cycles vs. run18 arm A's gentle 1↔2). The root cause is that the simulator's result is decoupled from the in-force allocation by the loop's own clock.

### Insight

`/latest` should mean *"give me the current performance estimate."* What that estimate **is** differs by backend, and server-sim already knows which mode it is in:

- **Real-vLLM (continuous):** the estimate is a **lookback** — the most-recent observed window. (unchanged)
- **Simulator (non-continuous):** the estimate is **computed on demand, now** — solve against the *current* in-force labels and return the result.

Pushing this decision into server-sim keeps the **collector completely evaluator-agnostic**: it always issues one `GET /latest` and always receives a fresh, in-force-coherent envelope. The collector, the coherence gate, and `pkg/evaluator` are untouched.

## 2. Goals / Non-Goals

**Goals**
- For non-continuous mode, `GET /latest` computes a fresh result from the current labels and the in-force M\*, synchronously, and returns it in the existing envelope shape.
- Eliminate the M\*-churn coherence-gate flip-flop for `blis`/`queue-analysis` by construction (the returned `effectiveInput.concurrency` always equals the in-force `maxbatchsize`).
- Leave continuous-mode behaviour (real-vLLM backends) byte-for-byte unchanged.
- Reuse the existing solve path (`solveWithPolicy`) so saturation policy, the `OfferedRPS` post-step, and result shape are identical across the loop and the on-demand path.

**Non-Goals**
- No change to the collector or any control-loop Go code. (The only control-loop change is a manifest env edit, out of scope here.)
- No removal or change of `POST /simulate` / `GET /simulate/:id` (the async job API) — it remains for manual/debug use.
- Not touching `watchAllocation` / allocation edge-detection — that serves the windowed `vllm-server` loop and is out of scope (see §7).
- No new tunables beyond reusing the existing `SERVERSIM_CONTINUOUS` switch.

## 3. Background: current `/latest` and loop wiring

`handleLatest` (`pkg/server/server.go`) serves `s.jobs.Latest()` — the most-recent completed job — or `404 "no result yet"` when the store is empty.

The job store is populated **only** by:
- the continuous loop (`runOnce` → `solveWithPolicy` → `jobs.Complete`), when `ContinuousMode` is true; or
- `POST /simulate` (the async job API), which nobody in the control loop calls anymore.

The collector (`control-loop/pkg/collector`) is `/latest`-only since `f8e0553`; the old `POST /simulate` + poll loop (`simulatePod`) was removed. **Therefore, with `SERVERSIM_CONTINUOUS=false` today, `/latest` returns `404` every cycle forever** — nothing populates the store. That is the bug this design fixes: it makes non-continuous mode a first-class, on-demand `/latest`.

The evaluator's `/solve` is **synchronous** (`evaluator.Client.SolveCtx` is a single blocking `POST /solve` that returns `AnalysisData` inline). So the on-demand path needs **no polling**: one blocking call computes the result. The blocking wait that used to be a poll loop in the collector becomes one blocking `SolveCtx` inside `handleLatest`.

## 4. Design

### 4.1 Factor the solve core out of `runOnce`

Extract the label→result core of `Loop.runOnce` into a reusable function that both the loop and the on-demand handler call. Proposed shape (in `pkg/server/`):

```go
// computeLatest reads the current downward-API labels, solves under the
// configured saturation policy, and returns the envelope fields. ok=false when
// the pod is not yet ready (labels missing/unparseable) — the caller maps that
// to a 404, matching cold-start semantics. err is a genuine solve failure.
func computeLatest(ctx context.Context, cfg config.Config, cli solver, labelsPath string)
        (eff evaluator.ProblemData, ad evaluator.AnalysisData, ok bool, err error) {
    labels, lerr := ReadLabels(labelsPath)
    if lerr != nil {
        return eff, ad, false, nil // not ready
    }
    pd, parsed := LabelsToProblemData(labels)
    if !parsed {
        return eff, ad, false, nil // not ready
    }
    eff, ad, err = solveWithPolicy(ctx, cli, cfg.SaturationPolicy, pd)
    if err != nil {
        return eff, ad, false, err
    }
    if ad.OfferedRPS > 0 { // continuous-vllm-server only; 0 for simulators
        eff.RPS = ad.OfferedRPS
    }
    return eff, ad, true, nil
}
```

`runOnce` is reworked to call `computeLatest` and then `jobs.Complete`/`jobs.Fail`, preserving its current logging and the `jobs.Create` id. No behavioural change to the loop.

### 4.2 Start the loop only in continuous mode

`Server.New` already guards loop startup on `cfg.ContinuousMode`. No change needed beyond confirming the loop never runs when the flag is false. (It already does not.)

### 4.3 On-demand `handleLatest`

```go
func (s *Server) handleLatest(c *gin.Context) {
    if s.cfg.ContinuousMode {
        // lookback: most-recent loop-completed window (unchanged)
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

    // non-continuous: compute on demand against current in-force labels.
    eff, ad, ok, err := computeLatest(c.Request.Context(), s.cfg, s.evalCli, s.labelsPath)
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
        return
    }
    if !ok {
        c.JSON(http.StatusNotFound, gin.H{"error": "no result yet"}) // pod not ready
        return
    }
    c.JSON(http.StatusOK, gin.H{
        "effectiveInput": eff,
        "result":         ad,
        "completedAt":    time.Now().UTC(),
    })
}
```

Notes:
- `s.labelsPath` is `filepath.Join(cfg.LabelsDir, "labels")` — the same path the loop uses; lift it to a `Server` field (or recompute) so both paths share it.
- **Cancellation:** pass `c.Request.Context()` into `solveWithPolicy`. When the collector's `GET /latest` timeout (`INFERNO_SIMULATE_TIMEOUT_SEC`) fires, the in-flight `/solve` is aborted cleanly rather than orphaned — mirroring how the loop cancels solves.
- **No caching.** The collector calls `/latest` once per pod per cycle; recomputing per call is correct and simplest. (A short TTL memo is possible future work but unnecessary now.)
- **Coherence by construction.** Because the envelope is built from labels read at request time, `effectiveInput.concurrency == ` the in-force `maxbatchsize` label, so the collector's coherence gate passes every cycle.

### 4.4 Envelope parity

Both branches emit the same three-field envelope (`effectiveInput`, `result`, `completedAt`). `completedAt` is `time.Now()` for the on-demand branch (the compute instant) vs. the job's stored completion time for the lookback branch — semantically identical ("when this estimate was produced").

## 5. Behaviour matrix

| Backend | `SERVERSIM_CONTINUOUS` | Loop runs? | `/latest` source | Coherence gate |
|---|---|---|---|---|
| `blis`, `queue-analysis` | `false` (new) | no | on-demand compute from current labels | always passes (in-force M\* by construction) |
| `vllm-server` (windowed) | `true` | yes | most-recent loop window (lookback) | gated until a post-decision window lands (unchanged) |
| `continuous-vllm-server` | `true` | yes | most-recent loop window (lookback) | trailing-window snapshot (unchanged) |

## 6. Control-loop side (contract, out of scope for code here)

- Set `SERVERSIM_CONTINUOUS=false` on the simulator deployments: `manifests/blis/dep-blis-qwen.yaml` (run19 target), and for consistency the other `blis`/`qa` deployments (`dep-blis-granite`, `dep-blis-llama`, `dep-qa-granite`, `dep-qa-llama`).
- `SERVERSIM_SATURATION_POLICY` keeps its current meaning on the on-demand path (run19 uses `pass-through`); `solveWithPolicy` is shared, so `retry-at-lower-load` behaves identically too.
- Operational note to update in control-loop docs: in non-continuous mode there is **no cold-start window** — `/latest` returns a result on the first call once the pod's labels file is populated; the only `404` is the brief pre-ready window before the downward-API volume projects.

## 7. Relationship to #25 (`cleanup/remove-watch-allocation`)

Independent. `watchAllocation` only affects the **loop**, which does not run for simulator backends under this design. We base this work on `main` and leave `watchAllocation` intact, because it still benefits the windowed `vllm-server` backend (used by `vllm-cpu`, still shipped). #25 is to be decided on real-vLLM merits separately.

## 8. Testing

- **Unit (`pkg/server`):**
  - `handleLatest` in non-continuous mode returns a computed envelope whose `effectiveInput.concurrency` equals the `maxbatchsize` in the labels file (the coherence invariant).
  - Non-continuous `handleLatest` with missing/unparseable labels → `404` (pod-not-ready), not `500`.
  - Non-continuous `handleLatest` with a solver error → `500`.
  - Continuous-mode `handleLatest` unchanged: serves `jobs.Latest()`, `404` when empty (regression guard).
  - `computeLatest` parity: same `(eff, ad)` the loop's `runOnce` would store for identical labels (factored-core equivalence).
  - Context cancellation: a cancelled request context aborts `solveWithPolicy` and surfaces (no publish, no hang).
- **`go test -race ./pkg/server/ ./pkg/job/`** stays green (no new shared mutable state on the on-demand path).
- **Cluster smoke (run19, separate effort):** deploy blis-qwen with `SERVERSIM_CONTINUOUS=false`; confirm steady replicas under the ramp (no 1→3→1 flips), no `stale result … holding` lines gating all pods, and non-zero deployment throughput every cycle.

## 9. Risks / open questions

- **blis solve latency.** Each `/latest` now triggers a synchronous DES solve. blis was fast in the run19 partial run and `queue-analysis` is ~instant; both must complete within the collector's per-pod `GET /latest` timeout, and `/collect` (parallel fan-out across pods) must fit the 120 s control period. Low risk given observed timings; the context-cancellation guard bounds the worst case.
- **`/solve` failure now surfaces as a `/latest` 500.** In continuous mode a failed window is logged and simply not published (collector sees staleness). On the on-demand path a solve failure returns `500`; the collector treats a failed `GET /latest` as "no result this cycle; skipping" — the same non-contributing outcome, so behaviour is equivalent. Confirm the collector logs this benignly.
- **Repeated `/latest` calls within a cycle** would each recompute. Not an issue today (one call per pod per cycle); revisit only if a caller polls.
- **Label-source skew (the asterisk on "coherence by construction").** server-sim computes `effectiveInput.concurrency` from the **pod's** downward-API labels file, while the collector's coherence gate compares against the **deployment's** `maxbatchsize` label. The Actuator writes both at end of cycle N; the downward-API volume projects the pod value with the usual kubelet sync delay. With a 120 s control period this projection completes well before the collector calls `/latest` at cycle N+1, so they agree — but a control period shorter than the projection delay could see one transient gated cycle. This is strictly less fragile than the continuous loop (no separate window clock to straddle), and it is the same propagation the loop already depends on; it is not a new risk introduced here.
```
