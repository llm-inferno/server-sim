# server-sim `/latest`, Evaluators, and Continuous Mode — Architecture Notes

> Reference notes on how server-sim, its evaluator backends, and the control-loop
> collector fit together around the `GET /latest` read path. Captures the
> architecture rationale behind the on-demand-`/latest` design
> (`docs/superpowers/specs/2026-06-27-on-demand-latest-noncontinuous-design.md`).
> Architecture only — no experiment specifics.

## Two classes of evaluator backend

All evaluators expose exactly one endpoint — **`POST /solve(ProblemData) → AnalysisData`** — and are stateless from server-sim's perspective. But they fall into two fundamentally different classes:

| Class | Backends | `/solve` nature |
|---|---|---|
| **Simulator** | `queue-analysis` (analytical), `blis` (DES), `dummy` | **Pure synchronous function** of `ProblemData`. Same input → same output, computed and returned inline. No background state, no wall-clock window. |
| **Real-vLLM** | `vllm-server` (windowed), `continuous-vllm-server` (persistent arrival loop) | Measures a **real** running vLLM over a wall-clock window. The result reflects the *observed past*, not a computed prediction. |

This distinction is the root of everything below: a simulator's performance estimate can be produced **on demand for any `(load, concurrency)`**, whereas a real-vLLM estimate can only be **observed over time**.

## `/solve` is synchronous; the async job API is a server-sim wrapper

A common point of confusion: which layer is async?

- **The evaluator's `/solve` is synchronous.** `evaluator.Client.SolveCtx` is a single blocking `POST /solve` that returns `AnalysisData` in the HTTP response body. There is no job ID and no polling at the evaluator layer. (`SolveCtx` is context-aware: cancelling the context aborts the in-flight request, which for real-vLLM stops the measurement window.)
- **The async job model lives only in server-sim's `/simulate`.** `POST /simulate` creates a job, returns a `jobID`, runs `evalCli.Solve` in a goroutine, and the caller polls `GET /simulate/:id`. This is the only place jobIDs and polling exist.

Consequence: any code path that needs a result can just make one blocking `SolveCtx` call. Polling is never required — it is an artifact of the `/simulate` wrapper, not of solving.

## The collector is `/latest`-only

Since control-loop commit `f8e0553`, the collector reads results **exclusively via `GET /latest`**. The earlier on-demand path — `POST /simulate` followed by a `GET /simulate/:id` poll loop in the collector (`simulatePod`) — was removed. The collector now does a single non-blocking `GET /latest` per pod, fanned out concurrently, governed by a per-pod timeout (`INFERNO_SIMULATE_TIMEOUT_SEC`).

This is why the *meaning* of `/latest` matters so much: it is the one and only interface between the collector and any backend. Keeping the collector evaluator-agnostic means all backend-specific behaviour must live behind `/latest` inside server-sim.

## Continuous mode and `/latest`

`SERVERSIM_CONTINUOUS=true` starts a background ticker loop (`pkg/server/loop.go`) that runs evaluation windows back-to-back: each tick reads the current workload + allocation from the downward-API labels file, builds a `ProblemData`, calls `solveWithPolicy`, and stores the result in the job store. `GET /latest` serves the **most-recent completed job** (a lookback).

This is the correct — and only workable — design for **real-vLLM** backends, where you must observe an actual arrival process over a window. The control loop relies on a few invariants here:

- **Causal-coherence gate (collector side):** the collector compares the window's `effectiveInput.concurrency` against the M\* currently in force (the pod/deployment `maxbatchsize` label). A mismatch means the window ran under an older allocation; the pod's reading is treated as stale and excluded for that cycle. Staleness is detected, never silent.
- **Control-period invariant (`vllm-server`):** `INFERNO_CONTROL_PERIOD` must exceed `warmupSec + maxWindowSec + slack` so a post-decision window completes within the cycle.
- **Allocation edge-detection (`watchAllocation`):** when M\* changes, the loop abandons the in-flight window so a fresh post-decision window starts immediately, rather than one full window later.

## Why continuous mode misfits simulators

For a **simulator**, wrapping the pure `/solve` in a background loop + job cache buys nothing and introduces a failure mode tied to the loop's independent clock:

1. The loop runs a window under the M\* in force when the window started.
2. The controller's Actuator rewrites the `maxbatchsize` label (new M\*).
3. `GET /latest` still serves the **stale** window (old M\*).
4. The collector's coherence gate rejects **every** reporting pod that cycle → deployment throughput collapses to 0 → `arrivalRate` falls back to the offered setpoint → the optimizer sees high-offered/zero-throughput and over-scales → it corrects the next cycle once a fresh window lands.

The visible symptom is **replica flip-flopping with transient zero-throughput cycles**. The deep cause is that a simulator's result has been *decoupled from the in-force allocation* by the loop's own clock — even though the simulator could have computed the right answer instantly.

## The fix in one sentence

`/latest` should mean **"give me the current performance estimate,"** and server-sim — which knows its own mode — decides what that estimate *is*:

- **Real-vLLM (continuous):** a **lookback** at the most-recent observed window. (unchanged)
- **Simulator (non-continuous):** **computed on demand, now** — solve against the current in-force labels and return it.

Because the simulator envelope is built from labels read *at request time*, `effectiveInput.concurrency` always equals the in-force `maxbatchsize`, so the coherence gate passes by construction and the flip-flop cannot occur. The collector, the coherence gate, and `pkg/evaluator` are untouched — the backend-specific decision is pushed entirely into server-sim's `handleLatest`. The blocking wait that used to be a collector poll loop becomes a single synchronous `SolveCtx` call inside `handleLatest` (with the request context threaded through, so the collector's `/latest` timeout aborts it cleanly).

## `watchAllocation` / allocation edge-detection (and PR #25)

`watchAllocation` only affects the **continuous loop**. Its trade-offs are backend-specific:

- **Windowed `vllm-server`:** edge-detection genuinely helps — without it, an M\* change lets the in-flight window finish under the old M\* (then gets coherence-gated and wasted), so a coherent post-decision observation can arrive up to ~2× `maxWindowSec` later, effectively doubling the control-period invariant.
- **`continuous-vllm-server`:** edge-detection is nearly pointless — the arrival loop is persistent and `/solve` returns a trailing-window snapshot, so there is no single window to "abandon"; the trailing window re-converges naturally.
- **Simulators:** irrelevant — the loop does not run for them under the on-demand design.

So removing `watchAllocation` (PR #25) is benign for `continuous-vllm-server` and irrelevant for simulators, but a real responsiveness regression for the still-shipped windowed `vllm-server` (used by the vllm-cpu workload). It is **correctness-safe everywhere** (the coherence gate is the safety valve — worst case is extra stale cycles), but it should be decided on real-vLLM merits, independently of the simulator on-demand change. The on-demand-`/latest` work is based on `main` and leaves `watchAllocation` intact.

## Quick reference: behaviour by backend

| Backend | `SERVERSIM_CONTINUOUS` | Loop runs? | `/latest` source | Coherence gate |
|---|---|---|---|---|
| `blis`, `queue-analysis` | `false` | no | on-demand compute from current labels | passes by construction |
| `vllm-server` (windowed) | `true` | yes | most-recent loop window (lookback) | gated until a post-decision window lands |
| `continuous-vllm-server` | `true` | yes | most-recent loop window (lookback) | trailing-window snapshot |
