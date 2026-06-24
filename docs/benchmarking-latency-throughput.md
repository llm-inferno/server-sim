# Benchmarking latency vs throughput on a live vLLM

`scripts/benchmark_curve.py` turns the `continuous-vllm-server` evaluator into a
benchmarking instrument. For a fixed workload (model, accelerator, token means,
`maxConcurrency`) it sweeps an increasing set of arrival rates and produces:

- **delay vs throughput** — `avgTTFT`, `avgITL`, `avgRespTime` vs `throughput`
- **avg concurrency vs throughput** — average in-flight derived via Little's law
- **drop fraction vs offered** — the saturation signal for this loss system

It drives the evaluator's `POST /solve` directly (the same `ProblemData` →
`AnalysisData` contract as every backend; see `pkg/evaluator/types.go`),
bypassing server-sim's noise/policy/job-store.

## Why this works (and the loss-system caveat)

The `continuous-vllm-server` evaluator runs a persistent open-loop Poisson
generator against the paired vLLM and reports a **trailing window** of metrics.
`POST /solve` both *reconfigures* the live load (`RPS`, `maxConcurrency`) and
*returns* the current window — exactly what a sweep needs.

Its admission limiter is **drop-if-full** (`continuous-vllm-server-evaluator/limiter.go`):
offered load beyond `maxConcurrency` is dropped, counted in `offeredRPS` but not
in `throughput`. So the server is a **loss system**, not an infinite queue —
latency stays bounded and **saturation appears as a throughput plateau with a
rising drop fraction** `1 − throughput/offeredRPS`, not a latency blow-up. The
throughput ceiling is roughly `maxConcurrency / mean_service_time`.

Two fields the backend does not provide, and how the driver fills them:

- **avg concurrency** is not reported → derived as `throughput × avgRespTime`
  (Little's law): the average admitted in-flight count ≈ effective batch size,
  which climbs toward `maxConcurrency` near the ceiling.
- **`maxRPS`** is always 0 for this backend → the range is found empirically
  (or seeded analytically), below.

## Choosing the arrival-rate range (`--seed-from`)

| Mode | Behaviour |
|------|-----------|
| `empirical` (default) | **Phase A** ramps `RPS` geometrically (`--ramp-factor`) from `--rps-seed` until a knee trips: drop fraction over `--drop-threshold`, a `saturation` flag, or TTFT past `--ttft-knee-mult ×` the low-load baseline. **Phase B** then sweeps `--points` rates up to `--alpha × R_knee` (α>1 puts the top points *past* the knee). |
| `analytic` | Skip Phase A; take `R_knee` from `--rps-knee`. Get the ceiling from the BLIS KV-cache / decode-bandwidth bounds — see [`docs/blis-overload-detection.md`](blis-overload-detection.md) (`λ·L_out > BW·TP/(Params·BytesPerParam)` and `λ·T_e2e·(L_in+L_out/2) > NumKVBlocks·BlockSize`). Useful as a cross-check against the empirical knee. |
| `manual` | Skip Phase A; sweep `--points` rates linearly in `[--rps-min, --rps-max]`. |

## Measurement protocol per point

For each target rate the driver POSTs the setpoint, **settles** for at least one
trailing window (`--window-sec`, which must match the evaluator's
`trailingWindowSec`; the first point also pays `--warmup-sec`), then averages
`--reads` window reads. The settle also guarantees `window × rps ≥ --min-samples`
so the evaluator returns a populated window rather than an empty one.

## Token distribution

Token **means** are per-request (`--in-tokens`, `--out-tokens`). The
distribution **shape** (`fixed`/`geometric`/`uniform`/`uniform-bounded`) is
config-scoped per `accelerator|model` entry in the evaluator's
`vllm-eval-config.json` and only changes on evaluator restart.

## Running it

The driver must be the **sole** `/solve` caller during a run. Deploy a minimal
paired stack (reuse the control-loop manifests) so nothing else drives load:

- vLLM pod + managed pod with the evaluator sidecar `args: ["continuous-vllm-server"]`.
- Set the server-sim sidecar `SERVERSIM_CONTINUOUS=false`, and do **not** deploy
  the load-emulator / controller.
- Pairing (evaluator → vLLM pod IP) is established by the Actuator's
  `inferno.server.pair-id` label, or set manually for a one-off.

Then port-forward and run:

```bash
kubectl port-forward -n <ns> pod/<managed-pod> 8081:8081 &

python3 scripts/benchmark_curve.py \
  --eval-url http://localhost:8081 \
  --model <model-key> --accelerator <accel-key> \
  --in-tokens 512 --out-tokens 256 \
  --max-concurrency 64 \
  --window-sec 30                # must match trailingWindowSec
```

Output lands in `scripts/benchmark_results/curve_<timestamp>.{csv,md,png}`
(PNG requires `matplotlib`; pass `--no-plot` to skip). Run
`python3 scripts/benchmark_curve.py --help` for the full flag list.

## Validation

This tool was used to produce real-vs-simulated latency–throughput curves for
Qwen2.5-14B-Instruct on an H100: it drove a live vLLM server (continuous-vllm-server
evaluator) and, unchanged, the BLIS and queue-analysis evaluators over the same
arrival-rate grid. The real server's measured ceiling (~7 req/s) was bracketed by
both simulators, confirming the sweep/settle protocol and the loss-system metrics
end to end. A full write-up with manifests, data, and graphs is kept as an example
run: [`experiments/qwen2.5-14b-h100/`](../experiments/qwen2.5-14b-h100/REPORT.md).
