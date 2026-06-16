# Data flow of `MaxConcurrency` (a.k.a. maxBatchSize / concurrency)

This note traces how the "maximum concurrent requests in the server" value flows
through server-sim and each evaluator backend. The concept appears under several
names across the codebase (`maxBatchSize`, `maxRunningReqs`, `concurrency`,
`maxConcurrency`), but inside server-sim it has exactly **one carrier**.

## The canonical field

`ProblemData.MaxConcurrency` (`int`, JSON key `maxConcurrency`) —
defined at `pkg/evaluator/types.go:18`, *"maximum concurrent requests in server."*

Every backend reads from this single field. The other names are **backend-local
aliases** for the same value.

## 1. Input — where the value comes from

server-sim itself never sets a default, env var, or hard-coded value for it. It is
purely a request field:

- `POST /simulate` → `pkg/server/server.go:48-52`: the JSON body is bound straight
  into `ProblemData` via `ShouldBindJSON`. The upstream caller (control loop /
  actuator) supplies `maxConcurrency`. If omitted, Go zero-values it to `0`.

There is no server-sim config knob for it (`pkg/config/config.go` has none).

## 2. Transformation within server-sim

**None.** It flows through untouched:

- `pkg/server/server.go:57` passes the whole `pd` to `evalCli.Solve(pd)`.
- `pkg/evaluator/client.go:28-34` marshals `pd` verbatim and POSTs it to the
  evaluator's `/solve`. No mutation, no clamping.
- Noise injection (`pkg/noise`) only touches **output** `AnalysisData` metrics,
  never `MaxConcurrency` (it is an input field, not part of `AnalysisData`).

## 3. Consumers — the backends, and the `0 → fallback` pattern

Each evaluator treats `pd.MaxConcurrency` as an **override** of a config-file
default, using `> 0` as the "present" sentinel:

| Backend | Local name | Default source (when `MaxConcurrency == 0`) | How it's consumed |
|---|---|---|---|
| **queue-analysis** | `maxBatchSize` | `serverConfig.MaxBatchSize` ← `model-data.json` `maxBatchSize` (`queue-analysis-evaluator/config.go:23,78`) | `handler.go:34-41`: `maxBatchSize = pd.MaxConcurrency` if `>0`, else config; fed to `qaAnalyzer.Configuration.MaxBatchSize` → analytical Markov model |
| **blis** | `maxRunningReqs` | `modelEntry.MaxRunningReqs` ← `blis-config.json` `maxRunningReqs` (`blis-evaluator/config.go:24`) | Two places: DES batch config `handler.go:60-62` → `NewBatchConfig(maxRunningReqs,...)`, and pre-sim KV-capacity saturation check `handler.go:183-189` |
| **vllm-server** | `Concurrency` | `defaultConcurrency` from vllm-eval-config.json, else `evaluator.DefaultMaxConcurrency` (256) | `handler.go` resolves via `evaluator.ResolveMaxConcurrency` → `windowParams.Concurrency` → `generator.go` `sem := make(chan struct{}, wp.Concurrency)`, a semaphore bounding in-flight requests to the real vLLM pod; excess Poisson arrivals are **dropped** |
| **dummy** | — | `evaluator.DefaultMaxConcurrency` (256) | `dummy-evaluator/main.go`: `MaxRPS = float32(concurrency) * 0.08` — directly scales reported capacity |

Fallback precedence is now uniform (see `maxconcurrency-defaults-design.md`):
`request value > 0` → `configured default > 0` → `evaluator.DefaultMaxConcurrency`
(logged). queue-analysis, vllm-server, and dummy all resolve through the shared
`evaluator.ResolveMaxConcurrency` helper. **blis is the one exception**: it validates
`maxRunningReqs > 0` at config load and fails loud, so it never reaches the backstop.

## 4. Output — does it propagate onward?

`MaxConcurrency` is **not** echoed in any response. `AnalysisData`
(`pkg/evaluator/types.go:26-39`) has no concurrency/batch field. The value
influences output only **indirectly**:

- It shapes computed metrics — most directly `MaxRPS` (dummy: linear scale; blis:
  bounds the KV-capacity ceiling; queue-analysis: feeds `MaxRate`).
- It can flip the `Saturation` field (e.g. blis `SaturationKV` at `handler.go:191`,
  dummy `SaturationOverload`).

Those derived metrics are what `GET /simulate/{id}` returns to the caller.

## Summary diagram

```
external client (maxConcurrency in JSON)
   │
   ▼  POST /simulate  → ShouldBindJSON → ProblemData.MaxConcurrency   [no transform]
pkg/server ──pd──▶ evaluator.Client.Solve ──pd (verbatim JSON)──▶ backend /solve
                                                                      │
        ┌──────────────────────────────────┬──────────────┬──────────┴──────────────┐
 queue-analysis: maxBatchSize       blis: maxRunningReqs   vllm: Concurrency        dummy: MaxRPS factor
 (else model-data.json,             (else blis-config.json, (else defaultConcurrency,(else 256
  else 256 backstop)                 validated > 0)          else 256 backstop)      backstop)
      │ Markov model                 │ DES batch + KV sat-check │ in-flight sem        │ MaxRPS = N*0.08
      └─────────────────────── influences AnalysisData (MaxRPS, Saturation, latencies) ┘
                                                                      │
            GET /simulate/{id} ◀── job store ◀── (noise on output metrics only) ◀────────┘
```

**Key takeaways:** it enters only via the HTTP request, passes through server-sim
**unchanged**, is interpreted by each backend as an optional override (`>0`) of a
backend-specific default, and is never returned directly — only reflected in
derived metrics like `MaxRPS` and `Saturation`.
