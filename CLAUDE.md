# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build ./...
```

Run server-sim (requires an evaluator already running on :8081):
```bash
EVALUATOR_URL=http://localhost:8081 go run ./cmd/server-sim
```

Run individual evaluators:
```bash
go run ./dummy-evaluator
go run ./queue-analysis-evaluator          # requires MODEL_DATA_FILE
cd blis-evaluator && BLIS_CONFIG_FILE=blis-config.json HW_CONFIG_FILE=/path/to/hardware_config.json go run .
# vllm-server (requires VLLM_EVAL_CONFIG_FILE and a paired vLLM pod)
VLLM_EVAL_CONFIG_FILE=vllm-server-evaluator/vllm-eval-config.json go run ./vllm-server-evaluator
```

Run tests:
```bash
go test ./...
```

Tests live alongside the code they cover (currently `vllm-server-evaluator/*_test.go`); other packages don't have tests yet.

## Architecture

server-sim is an async job broker that delegates to a pluggable evaluator backend:

1. `POST /simulate` — accepts `ProblemData`, spawns a goroutine that calls the evaluator, returns a `jobID`.
2. `GET /simulate/{id}` — polls for result (`pending` / `completed` / `failed`).
3. `GET /latest` — returns the most-recent completed job as `{"effectiveInput": …, "result": …, "completedAt": …}`; `404 {"error":"no result yet"}` on cold start.

### Continuous mode

When `SERVERSIM_CONTINUOUS=true`, server-sim starts a background ticker loop (`pkg/server/loop.go`) that runs evaluation windows back-to-back, one at a time. Each tick it reads the current workload (`rpm`, `intokens`, `outtokens`, `model`, `accelerator`, `maxbatchsize`) from the downward-API labels file at `<SERVERSIM_LABELS_DIR>/labels`, converts it to a `ProblemData`, and calls `solveWithPolicy` with the configured saturation policy. The saturation policy (`retry-at-lower-load` or `pass-through`) works identically to the on-demand path. When the `maxbatchsize` label changes between ticks, the in-flight window is cancelled immediately so the next window runs under the new concurrency without waiting for the old window to finish.

The result is stored in the job store and served by `GET /latest`. `POST /simulate` remains fully functional alongside continuous mode; the two paths coexist and share the same job store.

All backends implement the same `POST /solve` REST contract (`ProblemData` → `AnalysisData`). server-sim is backend-agnostic; evaluator-specific config (model parameters, hardware specs) is resolved internally by each evaluator from its own config file, never exposed in the request.

### Key packages (`pkg/`)

| Package | Role |
|---------|------|
| `config` | Env-var configuration for server-sim |
| `evaluator` | Shared types (`ProblemData`, `AnalysisData`) and HTTP client to `/solve` |
| `job` | In-memory async job store with TTL eviction |
| `noise` | Gaussian noise injection applied to `AnalysisData` after evaluation |
| `server` | Gin HTTP server wiring routes to job/evaluator/noise |

### Evaluator backends

| Directory | Approach |
|-----------|----------|
| `dummy-evaluator/` | Hardcoded metrics scaled by RPS — no config needed |
| `queue-analysis-evaluator/` | Analytical state-dependent Markovian model via `llm-inferno/queue-analysis`; loads Alpha/Beta/Gamma from `model-data.json` keyed by `acc`+`name` |
| `blis-evaluator/` | Discrete-event simulation via `inference-sim/BLIS`; loads KV/batch/hardware params from `blis-config.json`; latency backend controlled by `LATENCY_BACKEND` (default: `roofline`; also: `blackbox`, `crossmodel`, `trained-roofline`, `trained-physics`) |
| `vllm-server-evaluator/` | Drives a real paired vLLM server (open-loop Poisson + streaming TTFT/ITL); pairing established by control-loop Actuator via labels |

### Important invariants

- `throughput ≤ RPS` — server-sim clamps noisy throughput to RPS to preserve this.
- `maxConcurrency == 0` (omitted) means "use the server's native/configured concurrency". Evaluators resolve it uniformly via `evaluator.ResolveMaxConcurrency`: request value → per-model config default → `evaluator.DefaultMaxConcurrency` (256, logged). **Exception:** blis validates `maxRunningReqs > 0` at config load and fails loud, so it never reaches the backstop. See `docs/maxconcurrency-defaults-design.md`.
- `saturation != ""` from an evaluator means the server is overloaded; server-sim skips noise injection. `AnalysisData.IsSaturated()` is the canonical check. See `pkg/evaluator/types.go` for the `SaturationXxx` constants (`"bandwidth"`, `"kv_capacity"`, `"overloaded"`).
  - Metrics may be zero (BLIS pre-sim, DES was skipped) or populated with degraded-state values (queue-analysis, BLIS post-sim). `maxRPS` is populated where computable.
  - The `retry-at-lower-load` policy anchors the retry on `maxRPS` (re-runs at `maxRPS × util`). If a saturated result has no computable `maxRPS` (`<= 0`), the window **fails** instead of retrying at RPS 0 — the loop publishes nothing and the Collector treats the absence as staleness. `pass-through` is unaffected (it returns the saturated result as-is).
  - BLIS performs an analytical check *before* the DES using decode bandwidth and KV capacity bounds, avoiding expensive simulations on overloaded configs. All saturation checks apply a 2% tolerance margin (`saturationMargin = 0.98`).
- The evaluator HTTP client has a 10-minute wall-clock timeout (DES runs can be slow). The BLIS default simulation horizon is 300 s of *simulated* time — a longer horizon reduces cold-start throughput bias but increases wall-clock runtime; tune `simulationHorizon` in `blis-config.json` per entry if needed.

## Configuration

server-sim env vars (`pkg/config/config.go`):

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVERSIM_PORT` | `8080` | Listen port |
| `EVALUATOR_URL` | `http://localhost:8081` | Evaluator base URL |
| `NOISE_ENABLED` | `false` | Enable Gaussian noise |
| `NOISE_STD_FRACTION` | `0.05` | Noise std dev as fraction of metric |
| `JOB_TTL_MINUTES` | `60` | Job retention after completion |
| `SERVERSIM_CONTINUOUS` | `false` | Enable continuous evaluation loop (reads workload from labels file each tick) |
| `SERVERSIM_TICK_SECONDS` | `5` | Continuous loop tick interval in seconds (floor: 1) |
| `SERVERSIM_SATURATION_POLICY` | `retry-at-lower-load` | Saturation handling: `retry-at-lower-load` (re-simulate at progressively lower load up to 3 retries) or `pass-through` (return saturated result as-is). Unknown values are logged and fall back to the default. |
| `SERVERSIM_LABELS_DIR` | `/etc/podinfo` | Directory containing the downward-API `labels` file used by the continuous loop |

blis-evaluator additional vars: `BLIS_CONFIG_FILE`, `HW_CONFIG_FILE`, `LATENCY_BACKEND`, `EVALUATOR_PORT`.

queue-analysis-evaluator additional vars: `MODEL_DATA_FILE`, `DEFAULT_MAX_QUEUE_SIZE`, `EVALUATOR_PORT`.

vllm-server-evaluator additional vars: `VLLM_EVAL_CONFIG_FILE`, `POD_NAMESPACE`, `VLLM_NAMESPACE`, `EVALUATOR_PORT`.

## Module

`github.com/llm-inferno/server-sim` — part of the `llm-inferno` org. Uses Gin for HTTP (consistent with all llm-inferno repos).
