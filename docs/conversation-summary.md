# server-sim: Design Conversation Summary

This document captures the key decisions, rationale, and outcomes from the initial design and implementation conversation for server-sim.

---

## Project Intent

server-sim is a new repo in the llm-inferno family. Its purpose is to produce performance metrics (TTFT, ITL, goodput) for an LLM inference server under a given workload, without requiring a real server. It delegates evaluation to a pluggable backend — an analytical model, a discrete event simulator (DES), or an emulator.

---

## Phased Roadmap

| Phase | Evaluator | Status |
|-------|-----------|--------|
| 1 | Dummy (hardcoded metrics) | Complete |
| 2 | Analytical model (queue-analysis) | Complete |
| 3 | DES (inference-sim/BLIS wrapper) | Complete |

---

## Key Design Decisions

### 1. Evaluator interface: REST API (`POST /solve`)

**Decision:** Each evaluator backend exposes a REST `POST /solve` endpoint. server-sim is a REST client to the evaluator.

**Rationale:** Decouples server-sim from any specific evaluator implementation. Any backend (queue-analysis, DES, emulator) can be plugged in by wrapping it as a service implementing the same API. No Go import coupling between server-sim and the evaluator.

**Phase 3 implementation:** `blis-evaluator/` imports `github.com/inference-sim/inference-sim` as a Go library and calls `cluster.NewClusterSimulator` / `cs.Run()` directly. Config (`blis-config.json`) maps `accelerator|model` → BLIS simulation parameters (KV blocks, batch limits, HF model config path, hardware GPU name, alpha coefficients). Latency backend selected globally via `LATENCY_BACKEND` env var (default: `roofline`).

---

### 2. Async job-based API

**Decision:** server-sim's consumer API is async:
- `POST /simulate` → returns `201 Created` with a `jobID`
- `GET /simulate/{id}` → returns `pending`, `completed` (with metrics), or `failed`

**Rationale:** Evaluator backends vary dramatically in execution time — an analytical model responds in milliseconds, a DES can take seconds to minutes. A synchronous `POST → wait → response` would hang clients for slow backends. The async job pattern handles all backends uniformly and maps naturally to the future Kubernetes pod-label integration.

---

### 3. Evaluator API schema: matches `model-data.json` ecosystem

**Decision:** `ProblemData` (the evaluator request) contains workload characteristics (`RPS`, `AvgInputTokens`, `AvgOutputTokens`, `MaxConcurrency`) and server identity (`Accelerator`, `Model` as strings). Evaluator-specific parameters are resolved internally by the evaluator.

**Rationale:** The consumer should not need to know evaluator internals (Alpha/Beta/Gamma/MaxQueueSize). Using `Accelerator` and `Model` strings keeps the API backend-agnostic.

**Prior design removed:** Original `ProblemData` mirrored queue-analysis's schema directly (with `Alpha`, `Beta`, `Gamma`, `MaxQueueSize`). This was replaced because those fields are specific to the analytical model.

**`MaxBatchSize` → `MaxConcurrency`:** Renamed to be more semantically accurate and backend-agnostic.

**`MaxQueueSize` removed from ProblemData:** Specific to the analytical model; moved to the evaluator's internal config.

---

### 4. `model-data.json` as evaluator config

**Decision:** The queue-analysis evaluator reads `model-data.json` (via `MODEL_DATA_FILE` env var) to resolve `Accelerator + Model` → Alpha/Beta/Gamma parameters. This is the same JSON format already used across the llm-inferno ecosystem (same `acc`, `name`, `perfParms` structure).

**Rationale:** Reuses existing calibrated data rather than duplicating it in a new config format.

**`MaxQueueSize`:** Not in `model-data.json`; provided as a uniform default via `DEFAULT_MAX_QUEUE_SIZE` env var (default: 0, i.e. no external queue).

---

### 5. One evaluator instance serves all accelerator/model combinations

**Decision:** A single evaluator service instance loads a config file mapping multiple `accelerator + model` pairs to their parameters. If a requested combination is not in the config, it returns `400 Bad Request`.

**Alternative considered:** One instance per accelerator/model pair (configured via env vars). Rejected as it requires orchestration overhead for multiple combos.

---

### 6. Noise injection in server-sim, not in the evaluator

**Decision:** Gaussian noise is applied by server-sim after the evaluator responds, before storing the job result. Controlled by `NOISE_ENABLED` and `NOISE_STD_FRACTION` env vars.

**Rationale:** Keeps evaluator backends clean and deterministic. Noise is a consumer-level concern — it mimics modeling error and real-world disturbances for analytical model backends. Not needed for DES or emulators (which already produce realistic variation).

---

### 7. Pod-label Kubernetes integration deferred

**Decision:** The k8s pod-label interface (reading workload from pod labels, writing metrics back) is deferred to a later phase.

**Rationale:** The REST API works standalone and in containers. A thin k8s adapter on top can be added after the core evaluator pipeline is validated. Also, pod label values have a 63-character limit, which would restrict richer payloads.

---

### 8. queue-analysis used as a Go library (not a running service)

**Decision:** The `queue-analysis-evaluator` imports `github.com/llm-inferno/queue-analysis/pkg/analyzer` directly as a Go library. It does not call queue-analysis over HTTP.

**Rationale:** Avoids running two services for Phase 2. The `pkg/analyzer` package is fully decoupled from queue-analysis's HTTP layer and can be imported cleanly.

---

## What Was Built

### Phase 1

| File | Purpose |
|------|---------|
| `cmd/server-sim/main.go` | Entry point |
| `pkg/config/config.go` | Env-var config loading |
| `pkg/evaluator/types.go` | Shared `ProblemData` / `AnalysisData` types |
| `pkg/evaluator/client.go` | HTTP client calling evaluator `POST /solve` |
| `pkg/job/job.go` | In-memory async job manager |
| `pkg/noise/noise.go` | Gaussian noise injection |
| `pkg/server/server.go` | Gin REST API (`POST /simulate`, `GET /simulate/{id}`, `GET /health`) |
| `dummy-evaluator/main.go` | Standalone dummy evaluator (canned metrics, scales with RPS) |

### Phase 2

| File | Purpose |
|------|---------|
| `queue-analysis-evaluator/main.go` | Entry point; loads model-data.json, starts Gin server |
| `queue-analysis-evaluator/config.go` | JSON loader; builds `acc\|name` → `serverConfig` lookup map |
| `queue-analysis-evaluator/handler.go` | `POST /solve` handler: lookup → `LLMQueueAnalyzer.Analyze()` → `AnalysisData` |

### Phase 3

| File | Purpose |
|------|---------|
| `blis-evaluator/main.go` | Entry point; loads blis-config.json, reads `LATENCY_BACKEND`, starts Gin server |
| `blis-evaluator/config.go` | JSON loader; builds `accelerator\|model` → `modelEntry` lookup map with validation and defaults |
| `blis-evaluator/handler.go` | `POST /solve` handler: lookup → `cluster.NewClusterSimulator().Run()` → `AnalysisData` |
| `blis-evaluator/blis-config.json` | Sample config (two entries: H100 and A100 with granite-3.1-8b-instruct) |

---

## Configuration Reference

### server-sim

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVERSIM_PORT` | `8080` | Listen port |
| `EVALUATOR_URL` | `http://localhost:8081` | Evaluator backend URL |
| `NOISE_ENABLED` | `false` | Enable Gaussian noise on metrics |
| `NOISE_STD_FRACTION` | `0.05` | Noise std dev as fraction of metric value |
| `JOB_TTL_MINUTES` | `60` | Minutes to retain completed/failed jobs before eviction |

### dummy-evaluator

| Variable | Default | Description |
|----------|---------|-------------|
| `DUMMY_EVALUATOR_PORT` | `8081` | Listen port |

### queue-analysis-evaluator

| Variable | Default | Description |
|----------|---------|-------------|
| `MODEL_DATA_FILE` | `model-data.json` | Path to model-data.json |
| `DEFAULT_MAX_QUEUE_SIZE` | `0` | Default MaxQueueSize for all models (0 = no external queue) |
| `EVALUATOR_PORT` | `8081` | Listen port |

### blis-evaluator

| Variable | Default | Description |
|----------|---------|-------------|
| `BLIS_CONFIG_FILE` | `blis-config.json` | Path to blis-config.json |
| `HW_CONFIG_FILE` | `hardware_config.json` | Path to inference-sim hardware_config.json |
| `LATENCY_BACKEND` | `roofline` | Latency model: `roofline`, `blackbox`, `crossmodel`, `trained-roofline` |
| `EVALUATOR_PORT` | `8081` | Listen port |

---

## Open Items / Future Work

- **Kubernetes pod-label adapter:** A thin layer that reads workload from pod labels, calls `POST /simulate`, polls, and writes metrics back to labels.
- **`MaxConcurrency` default from model-data:** Currently, if `MaxConcurrency` is 0 in the request, the evaluator falls back to `maxBatchSize` from `model-data.json`. This could be made more explicit.
- **Overloaded RPS handling:** When RPS exceeds `maxRPS`, queue-analysis returns an error and the job shows `failed`. A possible improvement is to return the overloaded metrics anyway with a warning field, or to return `maxRPS` in the `AnalysisData` so the consumer can see the limit before submitting.
- **Persistent job store:** Jobs are currently in-memory (lost on restart). A persistent store (e.g., Redis or a simple file) could be added for durability.
