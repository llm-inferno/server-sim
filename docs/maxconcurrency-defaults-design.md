# Design: uniform evaluator behavior for unspecified `maxConcurrency`

Tracking issue: llm-inferno/server-sim#17.
Background data-flow trace: [`maxconcurrency-dataflow.md`](maxconcurrency-dataflow.md).

## Problem

`ProblemData.MaxConcurrency` (JSON `maxConcurrency`) is the single field carrying
"max concurrent requests in the server." server-sim passes it through unchanged;
each evaluator interprets it under a backend-local name. When the request omits it
(`0`), the four backends behave differently:

| Backend | `maxConcurrency == 0` → | Default source |
|---|---|---|
| queue-analysis | `maxBatchSize` from model-data.json | per-model config |
| blis | `maxRunningReqs` from blis-config.json | per-model config (validated `> 0`) |
| vllm-server | `64` | hard-coded magic number in `generator.go` |
| dummy | `MaxRPS = 0` (saturation check disabled) | none (degenerate) |

Two issues: vllm-server hides an arbitrary `64` deep in the load generator, and
qa/dummy can silently emit degenerate results when nothing supplies a value.

## Goals

- A documented, uniform **contract**: `maxConcurrency == 0` means "use the server's
  native/configured concurrency."
- vllm-server resolves its default **from config**, like qa/blis (no magic number).
- A uniform **last-resort backstop** so no backend produces a degenerate/crashing
  result when neither request nor config supplies a value — and the backstop is
  **loud**, never silent.

## Non-goals

- Reading the paired vLLM pod's real `max_num_seqs` (true vllm-server fidelity).
  Deferred; only matters if the client-side cap is observed to skew measurements.
- Mechanical sameness across backends. They are genuinely different (analytical
  model, DES, real pod, stub); we unify *mechanism and contract*, not values.

## Design

### Resolution order (qa, vllm, dummy)

```
request maxConcurrency > 0   → use it
else configured default > 0  → use it
else                         → evaluator.DefaultMaxConcurrency (log a warning)
```

### Shared helper (`pkg/evaluator`)

```go
// DefaultMaxConcurrency is the last-resort concurrency used when neither the
// request nor the evaluator's own configuration specifies a value. 256 matches
// vLLM's historical / A100-class max_num_seqs default; it is a conservative
// backstop, not a claim to match every pod (current vLLM uses 1024 on H100/H200).
const DefaultMaxConcurrency = 256

// ResolveMaxConcurrency picks the effective concurrency. who identifies the
// caller in the warning logged when the backstop fires (reaching it means
// neither the request nor config supplied a value — i.e. likely misconfig).
func ResolveMaxConcurrency(requested, configured int, who string) int
```

Logging lives in the helper (all evaluators are simple `main` binaries that
already use the standard logger), keeping the resolution logic in exactly one
place.

### blis is exempt by design

blis validates `maxRunningReqs > 0` at config load (`blis-evaluator/config.go`)
and fails loud on misconfiguration. It keeps that check and therefore never reaches
the backstop. We do **not** weaken the validation for the sake of uniformity — loud
failure is preferable to a hardware-inappropriate guess for the KV-bound DES.

## Implementation plan

1. **`pkg/evaluator/concurrency.go`** (new) — `DefaultMaxConcurrency` const +
   `ResolveMaxConcurrency`; **`concurrency_test.go`** — table test (request wins /
   config wins / backstop+log).
2. **`pkg/evaluator/types.go`** — document the `maxConcurrency == 0` contract on the
   field.
3. **queue-analysis-evaluator/handler.go** — replace the inline override with
   `ResolveMaxConcurrency(pd.MaxConcurrency, sc.MaxBatchSize, "queue-analysis")`.
4. **vllm-server-evaluator** — add `defaultConcurrency` to `configEntry` and
   `serverConfig` (passthrough in `loadConfig`); resolve in `handler.go` via the
   helper; replace the `64` literal in `generator.go` with
   `evaluator.DefaultMaxConcurrency` (defensive guard for direct callers). Add the
   field to the sample `vllm-eval-config.json`.
5. **dummy-evaluator/main.go** — resolve concurrency via the helper (configured `0`)
   before computing `MaxRPS`.
6. **blis-evaluator/handler.go** — add a comment noting blis is intentionally exempt
   (validation guarantees a positive configured value).
7. **Docs** — update `maxconcurrency-dataflow.md` fallback table; add the invariant
   to `CLAUDE.md`; document `defaultConcurrency` in `docs/vllm-server-evaluator.md`.

## Risk

- **Behavior unchanged by default**: omitting `defaultConcurrency` from vllm config
  resolves to `256` only when the request also omits the value; existing callers
  that pass `maxConcurrency` are unaffected. qa/blis paths with configured values
  are unchanged.
- **`generator_test.go`** passes `Concurrency` explicitly, so it is unaffected.
- The only observable change for a previously-degenerate path: dummy with
  `maxConcurrency == 0` now reports a finite `MaxRPS` (256 × 0.08 = 20.48) instead
  of `0`. A side effect is that the dummy saturation check — guarded by
  `MaxRPS > 0` — is now active for these requests, so `RPS > ~20` is flagged
  `overloaded` (previously the `MaxRPS = 0` suppressed the check entirely).
- dummy resolves the backstop inline (constant only), not via
  `ResolveMaxConcurrency`: a config-free stub reaches the backstop on every
  omitted-`maxConcurrency` request by design, so the helper's misconfiguration
  warning would be per-request noise.
