# server-sim

LLM server performance simulator. Given workload characteristics, produces performance metrics (TTFT, ITL, goodput) by delegating to a pluggable evaluator backend.

See [docs/design.md](docs/design.md) for architecture and API reference.

## Phase 1: Skeleton + Dummy Evaluator

### Prerequisites

- Go 1.24+

### Build

```bash
go build ./...
```

### Test Run

**Step 1 — start the dummy evaluator** (terminal 1):

```bash
go run ./dummy-evaluator
# Listening on :8081
```

**Step 2 — start server-sim** (terminal 2):

```bash
EVALUATOR_URL=http://localhost:8081 go run ./cmd/server-sim
# Listening on :8080
```

**Step 3 — exercise the API** (terminal 3):

```bash
# Health check
curl http://localhost:8080/health

# Submit a simulation job
curl -s -X POST http://localhost:8080/simulate \
  -H "Content-Type: application/json" \
  -d '{
    "RPS": 3.0,
    "maxConcurrency": 48,
    "avgInputTokens": 128,
    "avgOutputTokens": 512,
    "accelerator": "H100",
    "model": "llama-3-8b"
  }'
# → {"jobID":"<uuid>"}

# Poll for result (replace <uuid>)
curl -s http://localhost:8080/simulate/<uuid>
# → {"jobID":"...","status":"completed","result":{...}}
```

### Test Noise Injection

Restart server-sim with `NOISE_ENABLED=true` and repeat the submit/poll steps a few times — metrics will vary slightly each call:

```bash
EVALUATOR_URL=http://localhost:8081 NOISE_ENABLED=true go run ./cmd/server-sim
```

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVERSIM_PORT` | `8080` | server-sim listen port |
| `EVALUATOR_URL` | `http://localhost:8081` | Evaluator backend base URL |
| `NOISE_ENABLED` | `false` | Add Gaussian noise to metrics |
| `NOISE_STD_FRACTION` | `0.05` | Noise std dev as fraction of metric value |
| `DUMMY_EVALUATOR_PORT` | `8081` | Dummy evaluator listen port |

### Docker

```bash
docker build -t server-sim .
docker run -p 8080:8080 -e EVALUATOR_URL=http://<evaluator-host>:8081 server-sim
```

---

## Phase 2: Queue-Analysis Evaluator

Uses the [queue-analysis](https://github.com/llm-inferno/queue-analysis) analytical model. A YAML config file maps `accelerator + model` pairs to model parameters (Alpha, Beta, Gamma, MaxQueueSize).

### Test Run

**Step 1 — start the queue-analysis evaluator** (terminal 1):

```bash
MODEL_DATA_FILE=/path/to/model-data.json go run ./queue-analysis-evaluator
# Listening on :8081
```

The `model-data.json` file maps accelerator+model pairs to Alpha/Beta/Gamma parameters. See [sample-data](https://github.com/llm-inferno/sample-data) for examples.

**Step 2 — start server-sim** (terminal 2):

```bash
EVALUATOR_URL=http://localhost:8081 go run ./cmd/server-sim
```

**Step 3 — submit and poll** (terminal 3):

```bash
# Submit job (use names matching entries in your model-data.json)
curl -s -X POST http://localhost:8080/simulate \
  -H "Content-Type: application/json" \
  -d '{"RPS":1.0,"avgInputTokens":128,"avgOutputTokens":512,"accelerator":"A100","model":"llama_13b"}'
# → {"jobID":"<uuid>"}

# Poll for result
curl -s http://localhost:8080/simulate/<uuid>
# → {"status":"completed","result":{"avgTTFT":120.0,"avgITL":54.9,"maxRPS":1.31,...}}
```

Note: if RPS exceeds the model's maximum stable rate (`maxRPS`), the job will show `"status":"failed"`.

**Unknown accelerator/model** — the job will show `"status":"failed"`:

```bash
curl -s -X POST http://localhost:8080/simulate \
  -H "Content-Type: application/json" \
  -d '{"RPS":1.0,"avgInputTokens":128,"avgOutputTokens":512,"accelerator":"H100","model":"gpt-4"}'
```

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `MODEL_DATA_FILE` | `model-data.json` | Path to model-data.json |
| `DEFAULT_MAX_QUEUE_SIZE` | `128` | Default max queue size for all models |
| `EVALUATOR_PORT` | `8081` | Queue-analysis evaluator listen port |
