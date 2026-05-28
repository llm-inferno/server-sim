# vllm-server Evaluator — Design

**Status:** Draft for review
**Date:** 2026-05-28
**Scope:** server-sim repo only. The Actuator extensions in `control-loop/` are out of scope here; this doc specifies the contract those extensions must satisfy.

## 1. Motivation

server-sim today supports three evaluator backends: `dummy`, `queue-analysis` (analytical state-dependent Markovian model), and `blis` (DES). All three are pure functions of `ProblemData` — given the same input, they always return the same `AnalysisData`.

This design adds a fourth backend, **`vllm-server`**, whose `/solve` returns metrics measured against a *real* running vLLM server. The evaluator drives synthetic open-loop traffic at the requested RPS and collects TTFT, ITL, response time, and queue-time metrics from the vLLM under test.

Use case: when server-sim is deployed as a sidecar in autoscaled managed Deployments, this backend lets the control loop optimize against measured real performance rather than analytical predictions.

## 2. Architecture overview

The new evaluator runs as a sidecar in the managed Deployment pod (same pattern as today's evaluators) and is selected via `args: ["vllm-server"]` on the existing `evaluator` image. It implements the same `POST /solve(ProblemData) → AnalysisData` contract — **no changes to `pkg/evaluator`, server-sim, or job machinery**.

The fundamental shift vs. existing evaluators: solving is no longer a pure function of `ProblemData`. It requires a real vLLM pod paired 1:1 with the managed pod hosting the evaluator. Pairing is established by the **control-loop Actuator** (label-based), discovered by the evaluator via the K8s API, and verified before serving traffic.

```
control-loop                    server-sim (this repo)
   Actuator                      managed Deployment pod         vLLM Deployment pod
       │                       ┌─────────────────────────┐    ┌──────────────────┐
       │ scale + label both    │ server-sim   :8080      │    │  vllm-server     │
       ├──────────────────────►│ vllm-server-eval :8081  │───►│  :8000 (OpenAI)  │
       │                       │   (drives traffic,      │    └──────────────────┘
       │                       │    scrapes /metrics)    │             ▲
       │                       │ pair-id label  ─────────┼─────────────┘
       │                       └─────────────────────────┘     resolved via K8s API
       │
       └─► writes label `inferno.server.pair-id=<uuid>` on
           one managed pod and exactly one vLLM pod
```

### Topology decision: lockstep with label-based pairing

- **Both sides are plain Deployments** (no StatefulSet conversion) — preserves compatibility with queue-analysis/blis workloads.
- **Replica count is mirrored** by the Actuator — when managed Deployment scales N→M, paired vLLM Deployment scales N→M.
- **Per-pod pairing is by label** — the Actuator writes matching `inferno.server.pair-id=<uuid>` on exactly one pod on each side after both pods are Ready.
- **Cold-start / unreadiness** returns `503` from `/solve` so the Collector retries next cycle, matching its existing per-pod failure-tolerance behaviour.

### Responsibility split

- **server-sim repo (this work):** new `vllm-server-evaluator/` directory; K8s client to resolve the paired vLLM pod; Poisson load generator with streaming TTFT/ITL collection; Prometheus scrape for queue time; configurable measurement window with warmup.
- **control-loop repo (separate work):** Actuator extension that scales a paired vLLM Deployment and assigns `pair-id` labels per the four-invariant contract in §6.

## 3. Components

```
vllm-server-evaluator/
├── main.go            # gin server on :EVALUATOR_PORT, mounts /solve
├── config.go          # loads VLLM_EVAL_CONFIG_FILE
├── pairing.go         # K8s client: read pair-id from downward API, find vLLM pod
├── handler.go         # /solve handler
├── generator.go       # open-loop Poisson driver, streaming TTFT/ITL collection
├── metrics.go         # Prometheus scrape from vLLM /metrics
└── vllm-eval-config.json  # sample config
```

### Configuration (`vllm-eval-config.json`)

Keyed by `accelerator|model` like the other evaluators. Values describe measurement policy, not analytical params.

```json
{
  "configs": [
    {
      "accelerator": "H100",
      "model": "ibm-granite/granite-3.1-8b-instruct",
      "vllmServedModelName": "granite",
      "vllmPort": 8000,
      "warmupSec": 5,
      "minWindowSec": 20,
      "maxWindowSec": 300,
      "targetSamples": 200,
      "minSamples": 50,
      "ignoreEOS": true,
      "queueTimeMetric": "vllm:request_queue_time_seconds"
    }
  ]
}
```

If `vllmServedModelName` is empty, defaults to `pd.Model` at lookup time.

### Environment variables

| Var | Purpose |
|---|---|
| `EVALUATOR_PORT` | server listen port (matches other evaluators) |
| `VLLM_EVAL_CONFIG_FILE` | path to the config JSON |
| `POD_NAME`, `POD_NAMESPACE` | downward API; used to read pair-id label |
| `VLLM_NAMESPACE` | namespace of the paired vLLM Deployment (default: same as `POD_NAMESPACE`) |
| `KUBECONFIG` | only for local dev; in-cluster uses the ServiceAccount |

### RBAC

The evaluator's ServiceAccount needs `get,list` on `pods` in `VLLM_NAMESPACE` to resolve its paired vLLM pod IP. Manifest provided in `deploy/k8s/rbac-vllm-server.yaml`.

### Pairing resolution at startup (`pairing.go`)

1. Read `inferno.server.pair-id` from `/etc/podinfo/labels` (downward API mount).
2. Read `inferno.server.vllm-deployment` from the same mount.
3. K8s API: list pods in `VLLM_NAMESPACE` with label selector `inferno.server.pair-id=<uuid>`, filter to `Running` + `Ready`. Expect exactly one.
4. Cache the pod IP. Add a watch to refresh on pod replacement.
5. If pairing is not yet established (label missing) or no Ready vLLM pod is found, evaluator runs in "unpaired" state — `/solve` returns `503 {"error":"vllm pairing not ready"}`.

## 4. `/solve` data flow

```
Collector ──POST /simulate──► server-sim ──POST /solve──► vllm-server-evaluator
                                                                  │
                                                                  ▼
                                              ┌── pairing ready? ──┐
                                              │                    │
                                            no│                  yes│
                                              ▼                    ▼
                                       503 {pending}        acquire vLLM mutex
                                                                   │
                                                                   ▼
                                                        validate served model
                                                        (GET /v1/models)
                                                                   │
                                                          ┌────────┴────────┐
                                                          │  mismatch       │
                                                          ▼                 │
                                                     400 {error}            │ ok
                                                                            ▼
                                                            launch generator goroutine
                                                                            │
                                                            ┌───────────────┼───────────────┐
                                                            │               │               │
                                                            ▼               ▼               ▼
                                                  Poisson scheduler   per-request        ticker:
                                                  (rate=pd.RPS)       streaming POST     scrape /metrics
                                                  fan-out workers      to /v1/completions  every Ns
                                                            │               │               │
                                                            └───────────────┴───────────────┘
                                                                            │
                                                                            ▼
                                                              window: [warmup_end, t_end]
                                                              t_end = max(now + minWindowSec,
                                                                          warmup_end + targetSamples / pd.RPS)
                                                              capped by maxWindowSec
                                                                            │
                                                                            ▼
                                                              aggregate samples; release mutex
                                                                            │
                                                                            ▼
                                                                  return AnalysisData
```

### Per-request driver (`generator.go`)

- **Synthetic input:** `prompt_token_ids` of length `round(pd.AvgInputTokens)`, populated with randomized ids drawn from a small valid range (varies per request to avoid prefix-cache hits).
- **Output control:** `max_tokens = round(pd.AvgOutputTokens)`, `ignore_eos: true`, `stream: true`.
- **TTFT** = `t_first_chunk - t_send`.
- **ITL** = mean of consecutive chunk-arrival deltas, equally weighted across requests.
- **Response time** = `t_last_chunk - t_send`.
- Requests started before `warmup_end` are recorded but excluded from aggregates. Requests started inside the window but completing after `t_end` are still included (excluding them would bias toward fast requests).

### Concurrency

A mutex per paired vLLM URL serializes `/solve` calls so concurrent measurements never share the vLLM. The Collector calls one pod at a time per cycle in practice, so this should not add observable latency.

### Saturation detection

Three independent runtime signals; any one triggers `Saturation = SaturationOverload`:

1. **TTFT trend:** linear-regression slope of TTFT samples over the window indicates >50% growth from start to end.
2. **Queue dominance:** scraped `vllm:request_queue_time_seconds` mean exceeds `vllm:request_inference_time_seconds` mean over the window.
3. **Errors:** any `429` from vLLM, or ≥5% request error rate.

`MaxRPS` is left at 0 (drivers don't sweep capacity).

### Output mapping

| AnalysisData field | Source |
|---|---|
| `Throughput` | `samples_completed / window_sec`, capped at `pd.RPS` per existing invariant |
| `AvgRespTime` | mean of per-request e2e times in window (ms) |
| `AvgTTFT` | mean of per-request TTFTs in window (ms) |
| `AvgITL` | mean of per-request mean-ITLs in window (ms) |
| `AvgWaitTime` | mean of `vllm:request_queue_time_seconds` scraped at `t_end` |
| `MaxRPS` | 0 |
| `Saturation` | `SaturationOverload` if any signal triggered, else `""` |

### Failure modes

| Condition | Response |
|---|---|
| Unpaired or no Ready vLLM | `503 {"error":"vllm pairing not ready"}` |
| Served-model mismatch | `400 {"error":"vllm serves X, requested Y"}` |
| Insufficient samples within `maxWindowSec` (RPS too low) | `500 {"error":"insufficient samples: need N, got M"}` |
| Per-request connection failures | If <5% of requests fail, continue normally; if ≥5%, `Saturation = SaturationOverload` |
| `accelerator|model` not in config | `400 {"error":"unknown accelerator/model combination"}` (matches existing evaluators) |

## 5. K8s deployment

### New manifests in `deploy/k8s/`

| File | Purpose |
|---|---|
| `pod-vllm-server.yaml` | Standalone test pod (server-sim + vllm-server-evaluator) paired with a manually-deployed vLLM. For local iteration. |
| `configmap-vllm-server.yaml` | Holds `vllm-eval-config.json` |
| `rbac-vllm-server.yaml` | ServiceAccount + Role granting `get,list` on `pods` in the vLLM namespace, RoleBinding |
| `deployment-vllm-template.yaml` | Reference vLLM Deployment template (model, accelerator, `--served-model-name`, GPU resource request, `vllm:` Prometheus annotations). Not deployed automatically — Actuator follows it. |

### Managed-pod template additions

The managed-pod template (in `control-loop/yamls/workload/`) gains a downward-API volume:

```yaml
volumes:
  - name: podinfo
    downwardAPI:
      items:
        - path: pair-id
          fieldRef: { fieldPath: "metadata.labels['inferno.server.pair-id']" }
        - path: vllm-deployment
          fieldRef: { fieldPath: "metadata.labels['inferno.server.vllm-deployment']" }
```

mounted at `/etc/podinfo` in the evaluator container.

### Build/test path

- Add `RUN go build -o vllm-server-evaluator ./vllm-server-evaluator` to `Dockerfile.evaluator`.
- Extend the entrypoint dispatch to handle `args: ["vllm-server"]`.
- `vllm-server-evaluator/handler_test.go` uses `httptest.Server` to fake a vLLM emitting SSE chunks at controlled intervals — verifies TTFT/ITL extraction, window aggregation, saturation triggers, served-model-mismatch path, and the unpaired-503 path. No real vLLM in CI.

## 6. Contract with control-loop Actuator

Documented in `docs/vllm-server-evaluator.md`. The Actuator must satisfy four invariants:

1. **Replica lockstep.** For every managed Deployment with labels `inferno.server.evaluator=vllm-server` and `inferno.server.vllm-deployment=<vd>`, the Actuator scales Deployment `<vd>` to the same `spec.replicas` as the managed Deployment.
2. **Pairing labels.** After both Deployments are at the desired replica count, the Actuator labels exactly one managed pod and one vLLM pod with the same `inferno.server.pair-id=<uuid>`. Each pair-id is unique across both Deployments.
3. **Pairing reconciliation on pod replacement.** If any pod (either side) is replaced, the Actuator writes a fresh pair-id binding the new pod to its peer.
4. **Order.** vLLM Deployment scaled first; pair-id labels written only after the vLLM pod is `Ready`. The evaluator independently re-checks `Ready`, but the Actuator should not deliberately race.

If any invariant is violated, the evaluator returns 503 from `/solve` and the Collector excludes the pod that cycle. Graceful degradation; no data corruption.

## 7. Out of scope

- Implementing the Actuator extension (separate work in `control-loop/`).
- A vLLM Deployment operator/CRD.
- Changes to `queue-analysis` or `blis` evaluators.
- Tokenizer-aware prompt generation (synthetic token-id padding is sufficient for the existing `AvgInputTokens` contract).
- Full distribution support — `ProblemData` only carries means, and the driver uses fixed lengths matching those means, consistent with what the existing evaluators get.
- Dynamic measurement-window adjustment based on observed variance (could be a future enhancement; v1 uses static config).

## 8. Risks

1. **Cost.** Each `/solve` consumes real GPU time. With control-loop's 60s default cycle and a 30s measurement window, paired vLLMs are utilized ~50% just for telemetry. Mitigation is operational — increase the control-loop period when this backend is in use.
2. **Steady-state assumption.** Saturated measurements reflect the synthetic load only, not real production load. The control loop should treat saturated results as "overload detected at this RPS" and not as accurate degraded-state metrics. Matches existing semantics for queue-analysis/blis.
3. **Synthetic prompt artifacts.** Repeating the same token id may hit vLLM's prefix cache and skew TTFT. Mitigation: per-request randomized token ids in a small valid range.
4. **vLLM version drift.** Depends on `prompt_token_ids`, `ignore_eos`, and the `vllm:request_queue_time_seconds` Prometheus metric. The doc will record the minimum tested vLLM version once verified.
5. **Pairing consistency under churn.** Stale pod IPs from a lagging watch. Mitigation: short connection timeout + `Ready` re-check before `/solve`; treat connection-refused as stale pairing → 503.

## 9. Open issues

- Whether `vllmServedModelName` should be optional (default to `pd.Model`) — currently spec'd as defaulting; will validate in implementation.
- Choice of K8s client library (`client-go` is the obvious pick, used elsewhere in `llm-inferno`).
- Optional CLI helper to manually pair a managed pod to a vLLM pod for local dev (likely yes, useful for `pod-vllm-server.yaml`).
- Minimum tested vLLM version (to be filled in during implementation).
