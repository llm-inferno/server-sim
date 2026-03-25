# Kubernetes Deployment

## Overview

server-sim deploys as a **two-container pod** (sidecar pattern): the `server-sim` gateway and a backend evaluator share a pod and communicate over `localhost`. Both containers are built from separate images, with the evaluator image containing all three backends in a single image.

```
┌─────────────────────────────────────────┐
│  Pod: server-sim                        │
│                                         │
│  ┌──────────────┐   localhost:8081      │
│  │  server-sim  │ ──────────────────►   │
│  │  :8080       │   POST /solve         │
│  └──────────────┘                       │
│                  ◄──────────────────    │
│  ┌──────────────┐   AnalysisData JSON   │
│  │  evaluator   │                       │
│  │  :8081       │ ← args: ["blis"]      │
│  └──────────────┘                       │
└─────────────────────────────────────────┘
```

## Images

| Image | Dockerfile | Description |
|-------|-----------|-------------|
| `server-sim` | `Dockerfile.server-sim` | Gateway service, exposes `POST /simulate` async job API on port 8080 |
| `evaluator` | `Dockerfile.evaluator` | All three backend evaluators in one image; backend selected via container args |

### Building

```bash
docker build -f Dockerfile.server-sim -t server-sim:latest .
docker build -f Dockerfile.evaluator  -t evaluator:latest .
```

## Evaluator Backends

The `evaluator` image contains three binaries. The container `args` field selects which one runs:

| `args` | Binary | Notes |
|--------|--------|-------|
| `["dummy"]` | `dummy-evaluator` | No config files required |
| `["queue-analysis"]` | `queue-analysis-evaluator` | Requires `model-data.json` |
| `["blis"]` | `blis-evaluator` | Requires `blis-config.json`, `hardware_config.json`, HF config files |

## Pod Manifests

Ready-to-use pod specs are in `deploy/k8s/`:

| File | Evaluator | ConfigMap |
|------|-----------|-----------|
| `pod-dummy.yaml` | dummy | none |
| `pod-queue-analysis.yaml` | queue-analysis | `queue-analysis-config` |
| `pod-blis.yaml` | blis | `blis-config` |

### Deploying

```bash
# Dummy (no config needed)
kubectl apply -f deploy/k8s/pod-dummy.yaml

# Queue-analysis (populate ConfigMap first)
kubectl apply -f deploy/k8s/configmap-queue-analysis.yaml
kubectl apply -f deploy/k8s/pod-queue-analysis.yaml

# BLIS (populate ConfigMap first)
kubectl apply -f deploy/k8s/configmap-blis.yaml
kubectl apply -f deploy/k8s/pod-blis.yaml
```

## ConfigMaps

Runtime data files are supplied via ConfigMaps mounted at `/app/config/` inside the evaluator container.

### queue-analysis: `configmap-queue-analysis.yaml`

Populate `model-data.json` with accelerator/model performance parameters. See [sample-data](https://github.com/llm-inferno/sample-data) for the format.

### blis: `configmap-blis.yaml`

Populate:
- `blis-config.json` — per-model simulation parameters (see `blis-evaluator/blis-config.json` for the schema). Update `hfConfigPath` values to reference `/app/config/hf-configs/...`.
- `hardware_config.json` — GPU hardware specs from the inference-sim repo.
- HuggingFace `config.json` files for each model, one key per file.

> **Note:** ConfigMaps have a 1 MiB limit. If HF configs or hardware configs exceed this, use a PVC or an init container to fetch files at pod startup (see the `initContainers` section in `pod-blis.yaml`).

## Environment Variables

### server-sim container

| Variable | Default | Description |
|----------|---------|-------------|
| `EVALUATOR_URL` | `http://localhost:8081` | URL of evaluator sidecar — leave as-is for sidecar |
| `SERVERSIM_PORT` | `8080` | Listen port |
| `NOISE_ENABLED` | `false` | Add Gaussian noise to metrics |
| `NOISE_STD_FRACTION` | `0.05` | Noise std as fraction of metric value |
| `JOB_TTL_MINUTES` | `60` | TTL for completed/failed job records |

### evaluator container

| Variable | Default | Description |
|----------|---------|-------------|
| `EVALUATOR_PORT` | `8081` | Listen port |
| `MODEL_DATA_FILE` | `model-data.json` | queue-analysis only: path to model data |
| `BLIS_CONFIG_FILE` | `blis-config.json` | blis only: path to BLIS config |
| `HW_CONFIG_FILE` | `hardware_config.json` | blis only: path to hardware config |
| `LATENCY_BACKEND` | `roofline` | blis only: `roofline`, `blackbox`, `crossmodel`, `trained-roofline` |

## Pod Annotations

Pod annotations (not labels) can carry arbitrary metadata about the simulation run, such as input parameters or result summaries. Labels are limited to 63 characters and are intended for selection; annotations support up to 256 KB.

Example:
```yaml
metadata:
  annotations:
    serversim.llm-inferno/rps: "10.5"
    serversim.llm-inferno/model: "ibm-granite/granite-3.1-8b-instruct"
    serversim.llm-inferno/accelerator: "H100"
```

## Control-Loop Integration

server-sim is designed to integrate with the [llm-inferno/control-loop](https://github.com/llm-inferno/control-loop) as the **simulation backend for the Collector**. server-sim itself remains K8s-unaware — it exposes only its REST API and does not read or patch Deployment labels.

### Deployment topology

Each managed inference server Deployment pod runs a server-sim+evaluator sidecar alongside the inference server container:

```
Deployment pod (one per inference server):
┌────────────────────────────────────────────────────────┐
│  metadata.labels:                                      │
│    inferno.server.managed: "true"                      │
│    inferno.server.name / model / class                 │
│    inferno.server.allocation.accelerator               │
│    inferno.server.allocation.maxbatchsize              │
│    inferno.server.load.rpm                             │
│    inferno.server.load.intokens / outtokens            │
│                                                        │
│  containers:                                           │
│    inference-server  (:serving-port)                   │
│    server-sim        (:8080)  ─► evaluator (:8081)     │
└────────────────────────────────────────────────────────┘
```

The Load Emulator updates `inferno.server.load.*` labels independently to reflect the current workload.

### Enhanced Collector flow

The Collector is extended to call each managed pod's server-sim endpoint as part of its collection cycle:

1. List Deployments with `inferno.server.managed=true`
2. For each Deployment, read load and allocation labels → construct `ProblemData`
3. `POST pod-ip:8080/simulate` with `ProblemData`
4. Poll `GET pod-ip:8080/simulate/:id` until complete
5. Attach predicted `AnalysisData` (throughput, avgRespTime, avgTTFT, avgITL, maxRPS) to `ServerCollectorInfo`

### `ProblemData` label mapping

| Label | `ProblemData` field |
|-------|-------------------|
| `inferno.server.load.rpm` ÷ 60 | `RPS` |
| `inferno.server.allocation.maxbatchsize` | `MaxConcurrency` |
| `inferno.server.load.intokens` | `AvgInputTokens` |
| `inferno.server.load.outtokens` | `AvgOutputTokens` |
| `inferno.server.allocation.accelerator` | `Accelerator` |
| `inferno.server.model` | `Model` |

### Extended control-loop flow

```
Load Emulator  →  inferno.server.load.* labels

Collector  (reads labels + calls server-sim per pod)
    ↓  ServerCollectorInfo  (load + allocation + predicted metrics)
Controller
    ├──►  Model Tuner  (reacts to predicted perf metrics across pods — TBD)
    └──►  Optimizer    (scaling: accelerator, maxBatch, replicas)
              ↓  ServerActuatorInfo
         Actuator  →  patches inferno.server.allocation.* labels + spec.replicas
```
