# Local kind testing with CPU vLLM

This guide sets up a single-node [kind](https://kind.sigs.k8s.io/) cluster that
runs server-sim paired with a CPU-only vLLM instance (no GPU required).  It is
intended for functional testing only — throughput numbers will not be
representative of any real accelerator.

## Prerequisites

- `kind` and `kubectl` installed
- Docker running with at least **12 GB** of memory available to containers (vLLM AOT compilation peaks above 8 GB)
- Internet access for the first run (pulls `vllm/vllm-openai-cpu:latest-arm64` and the
  Qwen2.5-0.5B-Instruct model weights from HuggingFace, ~9 GB total)

## Step 1 — Create the cluster

```bash
kind create cluster --name inferno
```

## Step 2 — Build and load the application images

The pods use `imagePullPolicy: Never`, so images must be loaded into the kind
node's local image store before the pods are created.

### server-sim

```bash
docker build -t server-sim:latest .
kind load docker-image server-sim:latest --name inferno
```

### evaluator

`Dockerfile.evaluator` builds all backends into one image dispatched by
`evaluator.sh` (the pod selects the backend via `args: ["vllm-server"]`):

```bash
docker build -t evaluator:latest -f Dockerfile.evaluator .
kind load docker-image evaluator:latest --name inferno
```

### vLLM CPU image

This image is large (~8 GB).  Pull it once and load it into the kind node.
On Apple Silicon (arm64) use the arm64-specific tag:

```bash
docker pull vllm/vllm-openai-cpu:latest-arm64
kind load docker-image vllm/vllm-openai-cpu:latest-arm64 --name inferno
```

## Step 3 — Apply manifests

```bash
kubectl apply -f deploy/k8s/rbac-vllm-server.yaml
kubectl apply -f deploy/k8s/configmap-vllm-server-cpu.yaml
kubectl apply -f deploy/k8s/deployment-vllm-cpu.yaml
```

## Step 4 — Wait for vLLM to become ready

vLLM downloads the model weights from HuggingFace on first startup (~1 GB).
The readiness probe allows up to 8 minutes:

```bash
kubectl wait --for=condition=available deployment/vllm-qwen-cpu --timeout=600s
```

To watch progress:

```bash
kubectl logs -f deployment/vllm-qwen-cpu
```

Startup is complete when you see a line like:

```
INFO:     Application startup complete.
```

## Step 5 — Deploy server-sim

```bash
kubectl apply -f deploy/k8s/pod-vllm-server-cpu.yaml
kubectl wait --for=condition=ready pod/server-sim-vllm-server-cpu --timeout=60s
```

The evaluator sidecar polls for its paired vLLM pod every 15 seconds.  Look for
the pairing log line:

```bash
kubectl logs server-sim-vllm-server-cpu -c evaluator | grep "pairing resolved"
# pairing resolved: vLLM pod 10.x.x.x:8000 (pair-id=dev-cpu-pair-1)
```

## Step 6 — Send a test request

```bash
kubectl port-forward pod/server-sim-vllm-server-cpu 8080:8080 &

curl -s -X POST http://localhost:8080/simulate \
  -H 'Content-Type: application/json' \
  -d '{"accelerator":"cpu","model":"Qwen/Qwen2.5-0.5B-Instruct","RPS":0.5,"avgInputTokens":64,"avgOutputTokens":32}' \
  | jq .
```

The response contains a `jobID`.  Poll until `status` is `completed`:

```bash
JOB_ID=<jobID from above>
curl -s http://localhost:8080/simulate/$JOB_ID | jq .
```

A CPU evaluation at 0.5 RPS with the default config takes roughly 2–3 minutes
(30 s warmup + 60 s measurement window).

## Teardown

```bash
kubectl delete -f deploy/k8s/pod-vllm-server-cpu.yaml
kubectl delete -f deploy/k8s/deployment-vllm-cpu.yaml
kubectl delete -f deploy/k8s/configmap-vllm-server-cpu.yaml
kubectl delete -f deploy/k8s/rbac-vllm-server.yaml

kill $(lsof -ti tcp:8080,8081)   # stop any port-forwards

kind delete cluster --name inferno
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `pairing not yet resolved` in evaluator logs | vLLM pod not Ready yet | Wait for Step 4 to complete before applying Step 5 |
| vLLM pod stuck in `Init` / `CrashLoopBackOff` | OOM — Docker memory limit too low | Increase Docker memory to ≥ 8 GB in Docker Desktop settings |
| `no Ready vLLM pod with pair-id=dev-cpu-pair-1` | Labels not matching | Confirm `inferno.server.pair-id` label is present on the vLLM pod: `kubectl get pod -L inferno.server.pair-id` |
| Slow model download | Network speed | Set `HF_HOME` to a host-mounted volume to cache weights across pod restarts |
| `ImagePullBackOff` on server-sim or evaluator | Image not loaded into kind | Re-run the `kind load docker-image` commands from Step 2 |
| `unrecognized arguments: --device cpu` | CPU mode is baked into `vllm/vllm-openai-cpu`; the flag is rejected | Already removed from `deployment-vllm-cpu.yaml` |
| `multiple (2) Ready vLLM pods with pair-id=…` | The server-sim pod carries the `inferno.server.pair-id` label (needed for the downward API), so the pairing resolver finds both pods | Fixed in `pairing.go`: selector now also requires `inferno.vllm.model`, which only vLLM pods carry |
| `insufficient samples: need N, got 0` + vLLM returning 400 on `/v1/completions` | vLLM v0.22+ dropped `prompt_token_ids`; token IDs must be sent via the standard `prompt` field | Fixed in `generator.go` |
| `Available memory … less than desired CPU memory utilization` | Default `--gpu-memory-utilization 0.92` exceeds free host RAM (despite the flag name, on the CPU backend it controls host RAM fraction) | Already set to `0.5` in `deployment-vllm-cpu.yaml`; lower further if still failing |
| `OOMKilled` (exit code 137) after warmup | Torch AOT compilation peaks above the container memory limit | Already set to `10Gi` limit in `deployment-vllm-cpu.yaml`; also ensure Docker Desktop has ≥ 12 GB memory allocated |
