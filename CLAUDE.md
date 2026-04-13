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
```

There are no tests (`*_test.go` files) in this repo yet.

## Architecture

server-sim is an async job broker that delegates to a pluggable evaluator backend:

1. `POST /simulate` — accepts `ProblemData`, spawns a goroutine that calls the evaluator, returns a `jobID`.
2. `GET /simulate/{id}` — polls for result (`pending` / `completed` / `failed`).

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
| `queue-analysis-evaluator/` | Analytical M/G/1 model via `llm-inferno/queue-analysis`; loads Alpha/Beta/Gamma from `model-data.json` keyed by `acc`+`name` |
| `blis-evaluator/` | Discrete-event simulation via `inference-sim/BLIS`; loads KV/batch/hardware params from `blis-config.json`; latency backend controlled by `LATENCY_BACKEND` (default: `roofline`; also: `blackbox`, `crossmodel`, `trained-roofline`, `trained-physics`) |

### Important invariants

- `throughput ≤ RPS` — server-sim clamps noisy throughput to RPS to preserve this.
- `maxRPS = 0` from an evaluator means the evaluator is overloaded; server-sim skips noise injection and propagates the failure.
- The evaluator HTTP client has a 10-minute timeout (DES runs can be slow).

## Configuration

server-sim env vars (`pkg/config/config.go`):

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVERSIM_PORT` | `8080` | Listen port |
| `EVALUATOR_URL` | `http://localhost:8081` | Evaluator base URL |
| `NOISE_ENABLED` | `false` | Enable Gaussian noise |
| `NOISE_STD_FRACTION` | `0.05` | Noise std dev as fraction of metric |
| `JOB_TTL_MINUTES` | `60` | Job retention after completion |

blis-evaluator additional vars: `BLIS_CONFIG_FILE`, `HW_CONFIG_FILE`, `LATENCY_BACKEND`, `EVALUATOR_PORT`.

queue-analysis-evaluator additional vars: `MODEL_DATA_FILE`, `DEFAULT_MAX_QUEUE_SIZE`, `EVALUATOR_PORT`.

## Module

`github.com/llm-inferno/server-sim` — part of the `llm-inferno` org. Uses Gin for HTTP (consistent with all llm-inferno repos).
