# server-sim: LLM Server Performance Simulator

## Overview

server-sim produces performance metrics (TTFT, ITL, goodput, etc.) for an LLM inference server under a given workload. It delegates evaluation to a pluggable **evaluator backend**, keeping server-sim itself backend-agnostic.

```
Consumer (REST client)
       │
       ▼
  server-sim
  POST /simulate → job ID
  GET  /simulate/{id} → metrics
       │
       ▼
  Evaluator backend (REST service)
  POST /solve → AnalysisData
```

## Phased Roadmap

| Phase | Evaluator | Description |
|-------|-----------|-------------|
| 1 | Dummy | Skeleton + hardcoded metrics. Validates the full async job flow. |
| 2 | Analytical model | `queue-analysis-evaluator/` wraps [queue-analysis](https://github.com/llm-inferno/queue-analysis) as a Go library. Loads Alpha/Beta/Gamma from `model-data.json` (keyed by `acc`+`name`). MaxQueueSize defaults to 128 (`DEFAULT_MAX_QUEUE_SIZE`). Noise enabled via `NOISE_ENABLED=true`. |
| 3 | DES | `blis-evaluator/` wraps [inference-sim/BLIS](https://github.com/inference-sim/inference-sim) as a Go library. Loads KV/batch/hardware params from `blis-config.json` (keyed by `accelerator`+`model`). Latency backend controlled by `LATENCY_BACKEND` (default: `roofline`). |

## Architecture

### Async Job Model

Evaluator backends vary dramatically in execution time: an analytical model responds in milliseconds, while a DES can take seconds to minutes. server-sim uses an async job-based API to handle all backends uniformly:

1. `POST /simulate` — submits workload, returns `201 Created` with a `jobID`.
2. `GET /simulate/{id}` — polls for result. Returns `pending`, `completed` (with metrics), or `failed`.

This also maps naturally to the Kubernetes pod-label integration (future phase): write workload labels → poll until metrics labels appear.

### Evaluator Interface

All evaluator backends expose a single REST endpoint:

```
POST /solve
Content-Type: application/json

Request:  ProblemData   (workload characteristics + model parameters)
Response: AnalysisData  (performance metrics)
```

The `ProblemData` request is backend-agnostic: it carries workload characteristics (`RPS`, token counts) and server identity (`accelerator`, `model`). Evaluator-specific parameters (e.g. Alpha/Beta/Gamma for the analytical model) are resolved internally by the evaluator, never exposed to the consumer.

### Evaluator Configuration

Each evaluator backend loads its configuration at startup. The queue-analysis evaluator reads a `model-data.json` file (path via `MODEL_DATA_FILE` env var) containing Alpha/Beta/Gamma parameters keyed by `acc` (accelerator) and `name` (model). This is the same format used across the llm-inferno ecosystem (see [sample-data](https://github.com/llm-inferno/sample-data)).

When a `/solve` request arrives, the evaluator looks up the entry matching the requested `accelerator` and `model`. If no entry is found, it returns `400 Bad Request`. A single evaluator instance can serve any number of accelerator/model combinations.

### Noise Injection

For analytical model backends, server-sim applies Gaussian noise to each returned metric. This mimics realistic disturbances and modeling error. Noise is:
- Configured per metric as a fraction (standard deviation / mean)
- Disabled by default; enabled via `NOISE_ENABLED=true`
- Applied after the evaluator responds, before the job result is stored

## API Reference

### server-sim REST API

#### `POST /simulate`
Submit a simulation job.

**Request body** (`ProblemData`):

| Field | Type | Description |
|-------|------|-------------|
| `RPS` | float32 | Request arrival rate (requests/sec) |
| `maxConcurrency` | int | Maximum concurrent requests in server |
| `avgInputTokens` | float32 | Average input tokens per request |
| `avgOutputTokens` | float32 | Average output tokens per request |
| `accelerator` | string | Accelerator type (e.g. `"H100"`, `"A100"`) |
| `model` | string | LLM model name (e.g. `"llama-3-8b"`) |

**Response** `201 Created`:
```json
{"jobID": "550e8400-e29b-41d4-a716-446655440000"}
```

#### `GET /simulate/{id}`
Poll for job result.

**Response** `200 OK`:
```json
// pending
{"jobID": "...", "status": "pending"}

// completed
{"jobID": "...", "status": "completed", "result": {
  "throughput": 3.0,
  "avgRespTime": 10568.4,
  "avgWaitTime": 28.3,
  "avgTTFT": 75.3,
  "avgITL": 20.5,
  "maxRPS": 3.8
}}

// failed
{"jobID": "...", "status": "failed", "error": "evaluator unreachable"}
```

#### `GET /health`
Liveness check. Returns `200 OK`.

### Evaluator API (shared by all backends)

#### `POST /solve`
Evaluate performance at a given workload.

Request: `ProblemData` (same as above). Evaluator-specific parameters (e.g. Alpha/Beta/Gamma for the analytical model) are derived internally by the evaluator from `accelerator` and `model` via a config file loaded at startup.

**Response** `200 OK` (`AnalysisData`):

| Field | Type | Description |
|-------|------|-------------|
| `throughput` | float32 | Effective throughput (req/sec) |
| `avgRespTime` | float32 | Average response time (ms) |
| `avgWaitTime` | float32 | Average queueing time (ms) |
| `avgTTFT` | float32 | Average time-to-first-token (ms) |
| `avgITL` | float32 | Average inter-token latency (ms) |
| `maxRPS` | float32 | Maximum stable request rate |

## Configuration

server-sim is configured via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVERSIM_PORT` | `8080` | HTTP listen port |
| `EVALUATOR_URL` | `http://localhost:8081` | Evaluator backend base URL |
| `NOISE_ENABLED` | `false` | Enable Gaussian noise on metrics |
| `NOISE_STD_FRACTION` | `0.05` | Noise std dev as fraction of metric value |
| `JOB_TTL_MINUTES` | `60` | Minutes to retain completed/failed jobs before eviction |

## Repository Structure

```
server-sim/
  cmd/server-sim/main.go          # Entry point
  pkg/
    config/config.go               # Configuration loading
    evaluator/types.go             # Shared API types (ProblemData, AnalysisData)
    evaluator/client.go            # HTTP client to evaluator /solve
    noise/noise.go                 # Gaussian noise injection
    job/job.go                     # Async job manager (in-memory)
    server/server.go               # Gin REST server
  dummy-evaluator/
    main.go                        # Standalone dummy evaluator service
  queue-analysis-evaluator/
    main.go                        # Analytical model evaluator entry point
    config.go                      # model-data.json loader
    handler.go                     # POST /solve handler (queue-analysis library)
  blis-evaluator/
    main.go                        # DES evaluator entry point
    config.go                      # blis-config.json loader
    handler.go                     # POST /solve handler (inference-sim/BLIS library)
    blis-config.json               # Sample config (accelerator+model → BLIS params)
  docs/
    design.md                      # This document
  Dockerfile
  go.mod                           # github.com/llm-inferno/server-sim
```

## Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Evaluator interface | REST API (`POST /solve`) | Backend-agnostic; queue-analysis works as-is; DES/emulator wrapped as services |
| API schema | Accelerator + Model strings | Backend-agnostic; evaluators derive their own internal parameters via config |
| Async API | Job-based (POST + poll) | Handles backends from milliseconds to minutes uniformly |
| Noise injection | server-sim layer, not evaluator | Backends stay clean; noise is a consumer concern |
| HTTP framework | Gin | Consistent with all llm-inferno repos |
| Module path | `github.com/llm-inferno/server-sim` | Follows org convention |
| Pod labels (k8s) | Deferred to later phase | Core flow validated first |
