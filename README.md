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
