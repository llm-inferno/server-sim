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
| 2 | Analytical model | Use [queue-analysis](https://github.com/llm-inferno/queue-analysis) as evaluator. Add Gaussian noise to mimic modeling error. |
| 3 | DES | Wrap [inference-sim/BLIS](https://github.com/inference-sim/inference-sim) as evaluator service. |

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

The schema matches [queue-analysis](https://github.com/llm-inferno/queue-analysis) exactly, so queue-analysis works as a backend with no adapter. Other backends (DES, emulator) are wrapped as services implementing this same API.

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
| `maxBatchSize` | int | Maximum batch size |
| `avgInputTokens` | float32 | Average input tokens per request |
| `avgOutputTokens` | float32 | Average output tokens per request |
| `alpha` | float32 | Base iteration time (ms) |
| `beta` | float32 | Per-token prefill cost (ms/token) |
| `gamma` | float32 | Quadratic batch/token interaction (ms/token²) |
| `maxQueueSize` | int | Maximum queue size |

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
  "avgNumInServ": 31.6,
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

Request: `ProblemData` (same as above)

**Response** `200 OK` (`AnalysisData`):

| Field | Type | Description |
|-------|------|-------------|
| `throughput` | float32 | Effective throughput (req/sec) |
| `avgRespTime` | float32 | Average response time (ms) |
| `avgWaitTime` | float32 | Average queueing time (ms) |
| `avgNumInServ` | float32 | Average requests in system |
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
  docs/
    design.md                      # This document
  Dockerfile
  go.mod                           # github.com/llm-inferno/server-sim
```

## Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Evaluator interface | REST API (`POST /solve`) | Backend-agnostic; queue-analysis works as-is; DES/emulator wrapped as services |
| API schema | Match queue-analysis | No adapter needed for Phase 2 |
| Async API | Job-based (POST + poll) | Handles backends from milliseconds to minutes uniformly |
| Noise injection | server-sim layer, not evaluator | Backends stay clean; noise is a consumer concern |
| HTTP framework | Gin | Consistent with all llm-inferno repos |
| Module path | `github.com/llm-inferno/server-sim` | Follows org convention |
| Pod labels (k8s) | Deferred to later phase | Core flow validated first |
