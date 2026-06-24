# Example run — latency/throughput curve, Qwen2.5-14B-Instruct on H100

A worked end-to-end run of `scripts/benchmark_curve.py` against a **real vLLM
server** (continuous-vllm-server evaluator) on an OpenShift H100 cluster, plus a
**BLIS-simulated** curve for the same workload, for sim-vs-real comparison.

This directory is a frozen record of one experiment: the exact manifests used
(including local modifications), the server's KV calibration, the benchmark
outputs, and the step log below. It is documentation, not a reusable harness —
paths and pair-ids are specific to this run.

> **The narrative write-up — description, results, tables, and graphs — is in
> [`REPORT.md`](REPORT.md).** This README is the reproduction log / file index.

## Environment

- **Cluster:** OpenShift, `NVIDIA-H100-80GB-HBM3` nodes.
- **Namespaces:** `inferno-workload` (vLLM server + managed wrapper). The full
  control loop (`inferno-system`) is intentionally **not** deployed — see below.
- **Model:** `Qwen/Qwen2.5-14B-Instruct`, served as `qwen`, bf16,
  `--max-model-len 4096`, `--max-num-seqs 256`, `--gpu-memory-utilization 0.90`.
- **Evaluator key:** `model=qwen_2_5_14b`, `accelerator=H100` (from
  `vllm-server-eval-config`), `trailingWindowSec=90`.
- **Source repos:** `server-sim` (this repo, branch `try/benchmark-qwen`) and
  `control-loop` (manifests). Manifests here are copies/edits of
  `control-loop/manifests/vllm-gpu/`.

## Why this stack shape

`benchmark_curve.py` must be the **sole** caller of the evaluator's `/solve`
(it both sets the live setpoint and reads the trailing window). So we deploy a
**minimal, manually-paired** stack and omit the controller and load-emulator:

- vLLM server pod (`vllm-qwen-14b-gpu`).
- Managed wrapper pod (`vllm-qwen-14b-server`): server-sim + the
  `continuous-vllm-server` evaluator sidecar, with `SERVERSIM_CONTINUOUS=false`
  so server-sim's own loop doesn't also drive `/solve`.
- **Pairing** (evaluator → vLLM pod IP) is normally done by the Actuator via an
  `inferno.server.pair-id` label. With no controller, we set that label
  **manually** on both pods (pair-id `bench-qwen`).

## Three evaluators, three model keys (don't conflate)

| Track | Evaluator | Needs GPU? | model / accelerator key | Config source |
|-------|-----------|-----------|--------------------------|---------------|
| Real  | `continuous-vllm-server` | yes (real vLLM) | `qwen_2_5_14b` / `H100` | `vllm-server-eval-config` ConfigMap |
| Sim (DES) | `blis-evaluator` (`trained-physics`) | no | `Qwen/Qwen2.5-14B-Instruct` / `H100` | `blis-evaluator/blis-config.json` |
| Sim (analytic) | `queue-analysis-evaluator` | no | `qwen_2_5_14b` / `H100` | `inferno-data/vllm-gpu/model-data.json` (α/β/γ) |

The BLIS entry's `totalKVBlocks` was calibrated to the real server (step 2). The
queue-analysis α/β/γ are the run16-converged values previously fit to this same
real qwen server, so that curve is expected to track real most closely.

## Steps taken

### 1. Deploy the real vLLM server (H100)

Prerequisites already present on the cluster: PVC `vllm-models-cache` (weights
cached), `hf-token-secret`, `vllm-server-eval-config` ConfigMap, and the
`vllm-server-evaluator` RBAC.

Local edits to `control-loop/manifests/vllm-gpu/deployment-vllm-qwen.yaml`
(saved as `manifests/deployment-vllm-qwen.yaml` here):
- `--max-num-seqs 128` → **256** (128 risked capping below the KV-bound knee).
- Added pod-template label `inferno.server.pair-id: bench-qwen` (manual pairing).

```bash
oc apply -f manifests/deployment-vllm-qwen.yaml
```

Scheduling note: the deployment's nodeAffinity excludes 9 nodes reserved for
another team / with a dead GPU. The remaining allowed GPU nodes were initially
full or unavailable (one cordoned, one under disk-pressure); the pod sat
`Pending` ~12 min until a GPU freed on `pokprod-b93r39s0`, then came up Ready
(weights cached on the PVC).

### 2. Calibrate the BLIS config from the server's KV report

vLLM v0.21.0 prints the real KV capacity at startup
(`results/vllm-startup-calibration.txt`):

```
Available KV cache memory: 41.43 GiB
GPU KV cache size: 226,240 tokens
Maximum concurrency for 4,096 tokens per request: 55.23x
```

→ `totalKVBlocks = 226,240 / blockSizeTokens(16) = 14,140`.

Updated the `Qwen/Qwen2.5-14B-Instruct` entry in
`blis-evaluator/blis-config.json` (branch `try/benchmark-qwen`):
`totalKVBlocks 6144 (estimate) → 14140 (measured)`, `maxModelLen 0 → 4096` (to
match the real server). The 55.23× line is the key insight: at full 4096-token
requests only ~55 fit in KV, far below `--max-num-seqs 256`, so the real knee is
**KV-bound and workload-dependent**, not the batch cap.

### 3. Deploy the managed wrapper (manually paired)

Local edits to `dep-vllm-qwen-server.yaml` (saved here):
- `SERVERSIM_CONTINUOUS: "true"` → **`"false"`** (driver must be the sole `/solve` caller).
- Added pod-template label `inferno.server.pair-id: bench-qwen`.
- `maxbatchsize` label `128` → `256` (informational for a manual run).

```bash
oc apply -f manifests/dep-vllm-qwen-server.yaml
oc rollout status deployment/vllm-qwen-14b-server -n inferno-workload
oc logs -n inferno-workload deployment/vllm-qwen-14b-server -c evaluator | grep 'pairing resolved'
# -> pairing resolved: vLLM pod 10.129.6.231:8000 (pair-id=bench-qwen)
```

### 4. Real-GPU sweep

```bash
oc port-forward -n inferno-workload deployment/vllm-qwen-14b-server 8081:8081 &
python3 scripts/benchmark_curve.py --eval-url http://localhost:8081 \
  --model qwen_2_5_14b --accelerator H100 \
  --in-tokens 1024 --out-tokens 512 --max-concurrency 256 \
  --window-sec 90 --seed-from empirical --rps-seed 1.0 --points 10 \
  --out-dir docs/example-runs/qwen2.5-14b-h100/results
```

Result (`results/curve_20260624_114204.{csv,md,png}`): **throughput ceiling ≈ 7 req/s**,
knee at rps ≈ 7.6 (flagged `overloaded`, concurrency ~134, TTFT spiking). The loss
system plateaus throughput with a rising drop fraction past the knee.

### 5. Patch the BLIS pre-check, then BLIS-simulated sweep + comparison

Running BLIS for the *same* workload first hit a wall: the analytical
**saturation pre-check** vetoed it at **rps ≈ 0.22**, returning `saturation:
"bandwidth"`, throughput 0 — ~30× below the real ceiling. Two over-conservative
bounds were responsible:

- **Bandwidth**: compared aggregate demand `rps×L_out` against the *batch-size-1*
  weight-streaming rate `BW/weightBytes ≈ 113 tok/s`, ignoring that a real engine
  decodes a whole batch per weight stream.
- **KV**: used worst-case `maxConcurrency×meanTokens` (256×1536 > capacity), so it
  saturated at *any* rps even though vLLM dynamically caps the batch.

The DES itself was fine (a benign small config simulated correctly) — only the
pre-check vetoed. Fix (`blis-evaluator/handler.go`, `checkSaturation`): a single
**batch-aware** decode-bandwidth ceiling —

```
avgContext = L_in + L_out/2
B          = min(maxRunningReqs, TotalKVSlots/avgContext)   # KV limits the batch
t_step     = (weightBytes + B*avgContext*kvBytesPerToken) / (BW*TP)
decodeTPS  = B / t_step
saturated if rps*L_out > decodeTPS*0.98 ; maxRPS = decodeTPS/L_out
```

degenerate `kv_capacity` only when a single average request can't fit
(`B_kv < 1`). For this workload the patched bound gives maxRPS ≈ 15.6 — ~2×
*optimistic* vs the real ~7 knee, but it no longer falsely vetoes the operating
range, so the DES runs through it (the DES then flags overload itself). Tests in
`blis-evaluator/saturation_test.go` were updated to the new physics plus a
regression test for the high-`maxConcurrency` false positive.

BLIS sweep (one-shot DES, no trailing window; `trained-physics` backend per its
generic trained weights):

```bash
cd blis-evaluator
LATENCY_BACKEND=trained-physics EVALUATOR_PORT=8091 \
  BLIS_CONFIG_FILE=blis-config.json HW_CONFIG_FILE=hardware_config.json go run . &
cd ..
python3 scripts/benchmark_curve.py --eval-url http://localhost:8091 \
  --model Qwen/Qwen2.5-14B-Instruct --accelerator H100 \
  --in-tokens 1024 --out-tokens 512 --max-concurrency 256 \
  --seed-from manual --rps-min 0.7594 --rps-max 9.8719 --points 10 \
  --window-sec 0 --margin-sec 0 --min-samples 0 --reads 1 --warmup-sec 0 \
  --out-dir docs/example-runs/qwen2.5-14b-h100/results/blis-sim

python3 docs/example-runs/qwen2.5-14b-h100/compare_curves.py \
  real=docs/example-runs/qwen2.5-14b-h100/results/curve_20260624_114204.csv \
  blis=docs/example-runs/qwen2.5-14b-h100/results/blis-sim/curve_20260624_121115.csv \
  qa=docs/example-runs/qwen2.5-14b-h100/results/qa-sim/curve_20260624_121825.csv \
  --out-dir docs/example-runs/qwen2.5-14b-h100/results
```

### 6. Queue-analysis (analytic) sweep

A third, model-based curve (no GPU, instant). The evaluator resolves α/β/γ by
`acc|name`; request `maxConcurrency` (256) overrides the file's `maxBatchSize`.

```bash
MODEL_DATA_FILE=<control-loop>/inferno-data/vllm-gpu/model-data.json \
  EVALUATOR_PORT=8092 go run ./queue-analysis-evaluator &
python3 scripts/benchmark_curve.py --eval-url http://localhost:8092 \
  --model qwen_2_5_14b --accelerator H100 \
  --in-tokens 1024 --out-tokens 512 --max-concurrency 256 \
  --seed-from manual --rps-min 0.7594 --rps-max 9.8719 --points 10 \
  --window-sec 0 --margin-sec 0 --min-samples 0 --reads 1 --warmup-sec 0 \
  --out-dir docs/example-runs/qwen2.5-14b-h100/results/qa-sim
```

## Findings

- **Throughput**: all three agree closely up to the knee. Real plateaus at ~7 req/s
  (knee ~7.6); queue-analysis tracks real almost exactly and plateaus ~8 (its
  maxRPS=8.1); BLIS tracks within ~10–15% and saturates ~8.
- **ITL**: BLIS and queue-analysis both match real well in the normal range (e.g. at
  rps≈5.8: real 27, blis 22, qa 27 ms). Near/after the knee queue-analysis slightly
  over-predicts ITL and BLIS slightly under-predicts.
- **TTFT**: **queue-analysis is much closer to real** (66–164 ms vs real 70–350 ms)
  than BLIS (39–62 ms — under-modeled). BLIS's queueing `alphaCoeffs` are generic
  defaults; queue-analysis's α/β/γ were fit to this server. Neither reproduces the
  real TTFT blow-up magnitude past saturation (real → 8–11 s), since both are
  stable-state models.
- **Calibration is the dominant factor**: the queue-analysis curve wins on TTFT purely
  because its parameters were fit to this exact server. The takeaway is less
  "analytic beats DES" than "a model calibrated to the target tracks it best".
- **The BLIS pre-check patch is essential**: without it BLIS vetoes this (or any
  realistic batched) workload at ~0.22 rps. Candidate for a standalone PR.
- `totalKVBlocks` was read from the live server (226,240 tokens → 14,140 blocks),
  not estimated; the first estimate (6144) was ~2× low.

See `results/comparison.md` and `results/comparison.png` for the 3-way overlay.

## Files

- `manifests/deployment-vllm-qwen.yaml` — vLLM server as deployed (256 seqs, pair-id label).
- `manifests/dep-vllm-qwen-server.yaml` — managed wrapper as deployed (CONTINUOUS=false, pair-id).
- `manifests/configmap-vllm-eval.yaml`, `manifests/rbac-vllm-eval.yaml` — eval config + RBAC (provenance).
- `compare_curves.py` — overlays the two CSVs into `comparison.{md,png}`.
- `manifests/model-data-qa.json` — queue-analysis α/β/γ for qwen/H100 (provenance copy
  of `control-loop/inferno-data/vllm-gpu/model-data.json`).
- `results/vllm-startup-calibration.txt` — raw KV calibration lines from the vLLM log.
- `results/curve_*.{csv,md,png}` — real-GPU sweep.
- `results/blis-sim/curve_*.{csv,md,png}` — BLIS-simulated sweep (trained-physics).
- `results/qa-sim/curve_*.{csv,md,png}` — queue-analysis sweep.
- `results/comparison.{md,png}` — 3-way overlay.

> The BLIS config used here (`blis-evaluator/blis-config.json`, entry
> `Qwen/Qwen2.5-14B-Instruct`) and the `checkSaturation` patch live in the repo on
> branch `try/benchmark-qwen`, not in this directory.
