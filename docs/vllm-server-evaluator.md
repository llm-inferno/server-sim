# vllm-server Evaluator

The `vllm-server` evaluator implements the server-sim `POST /solve` API by
driving a real vLLM server with synthetic open-loop traffic and reporting
measured TTFT, ITL, response time, throughput, and queue time.

This document is the operational reference. The full design rationale lives
in [`docs/superpowers/specs/2026-05-28-vllm-server-evaluator-design.md`](superpowers/specs/2026-05-28-vllm-server-evaluator-design.md).

## How it differs from other evaluators

`dummy`, `queue-analysis`, and `blis` are pure functions of `ProblemData`.
`vllm-server` is not — every `/solve` call requires a real vLLM pod paired
1:1 with the evaluator's pod. Pairing is established by the **control-loop
Actuator** via labels and discovered by the evaluator via the K8s API.

## Configuration

`vllm-eval-config.json` is keyed by `accelerator|model`. See
`vllm-server-evaluator/vllm-eval-config.json` for an example.

| Field | Description |
|---|---|
| `accelerator`, `model` | lookup key (matches `ProblemData.Accelerator/Model`) |
| `vllmServedModelName` | value of vLLM's `--served-model-name`; defaults to `model` |
| `vllmPort` | vLLM container port |
| `warmupSec` | discarded prefix at start of each window |
| `minWindowSec`, `maxWindowSec` | window bounds (sec) |
| `targetSamples` | samples to aim for; if not reached by `maxWindowSec`, the window ends |
| `minSamples` | below this, `/solve` returns 500 with insufficient-samples |
| `ignoreEOS` | passed through to vLLM |
| `queueTimeMetric` | name of the queue-time histogram metric (used in scrape) |

## Environment variables

| Var | Default | Description |
|---|---|---|
| `EVALUATOR_PORT` | `8081` | listen port |
| `VLLM_EVAL_CONFIG_FILE` | `vllm-eval-config.json` | config path |
| `POD_NAME`, `POD_NAMESPACE` | from downward API | identifies the evaluator's own pod |
| `VLLM_NAMESPACE` | `POD_NAMESPACE` | namespace where paired vLLM Deployments live |

## Pairing — Actuator contract

The control-loop Actuator MUST satisfy these invariants:

1. **Replica lockstep.** For every managed Deployment with labels
   `inferno.server.evaluator=vllm-server` and
   `inferno.server.vllm-deployment=<vd>`, the Actuator scales Deployment `<vd>`
   to the same `spec.replicas` as the managed Deployment.

2. **Pairing labels.** After both Deployments are at the desired replica count,
   the Actuator labels exactly one managed pod and one vLLM pod with the same
   `inferno.server.pair-id=<uuid>`. Each pair-id is unique across both
   Deployments.

3. **Pairing reconciliation on pod replacement.** If any pod (either side) is
   replaced, the Actuator writes a fresh pair-id binding the new pod to its
   peer.

4. **Order.** vLLM Deployment scaled first; pair-id labels written only after
   the vLLM pod is `Ready`.

If any invariant is violated, the evaluator returns 503 from `/solve` and the
Collector excludes the pod that cycle. Graceful degradation; no data
corruption.

## Failure modes

| Condition | HTTP response |
|---|---|
| Unpaired or no Ready vLLM | 503 |
| Served-model mismatch | 400 |
| Insufficient samples within `maxWindowSec` | 500 |
| Per-request connection failures (≥5%) | 200 with `Saturation: overloaded` |
| `accelerator\|model` not in config | 400 |

## Saturation signals (any one triggers `Saturation: overloaded`)

1. TTFT linear-regression growth >50% across the window.
2. `vllm:request_queue_time_seconds` mean > `vllm:request_inference_time_seconds` mean.
3. ≥5% request error rate, or any 429 from vLLM.

## Local development

Use `deploy/k8s/pod-vllm-server.yaml` as a starting point. You'll need to:

1. Apply `deploy/k8s/rbac-vllm-server.yaml` to create the ServiceAccount.
2. Apply `deploy/k8s/configmap-vllm-server.yaml`.
3. Manually deploy a vLLM pod with `inferno.server.pair-id=MANUAL-DEV-PAIR-1`
   on the same node, in the namespace pointed to by `VLLM_NAMESPACE`.
4. Apply `deploy/k8s/pod-vllm-server.yaml`.
5. `kubectl port-forward server-sim-vllm-server 8080:8080` and POST to
   `/simulate` as you would with any other evaluator.
