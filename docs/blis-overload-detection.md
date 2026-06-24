# Overload Detection

This guide explains how to determine whether a simulated server will be in an overload
(saturation) condition — either analytically before running the simulator, or by reading
specific indicators from simulator output.

## Why overload detection matters

When a server is saturated, reported latency metrics (TTFT, ITL, E2E) become unreliable:
the queue grows unboundedly and reported averages reflect queue buildup rather than
intrinsic serving latency. Identifying saturation before interpreting results is essential
for valid capacity planning.

---

## Method 1: Analytical pre-simulation check

The trained-physics and roofline latency models expose all parameters needed to compute a
theoretical throughput ceiling. No DES run is required.

### Two independent bottlenecks

#### Bottleneck A — Memory bandwidth (decode-dominated workloads)

During steady-state decode, each step must stream model weights once across all requests
in the batch. The weight-streaming bound on decode throughput is:

```
decode_capacity_tokens_per_sec =
    (BW_peak_TB_s × TP × 1e12) / (NumParams × WeightBytesPerParam)
```

KV cache reads add on top of weights and dominate at long sequences, making the actual
capacity lower. A tighter bound that includes KV:

```
T_step_decode = (weight_bytes/TP
                 + 2 × NumLayers × NumKVHeads × HeadDim × BytesPerParam
                   × avg_kv_len × BatchSize)
                / (BW_peak_TB_s × 1e12)

max_decode_tput_tokens_per_sec = BatchSize / T_step_decode
```

**Overload condition:** `λ × L_out > max_decode_tput_tokens_per_sec`

> **Implementation note (server-sim `blis-evaluator`).** `checkSaturation` in
> `blis-evaluator/handler.go` uses the **batch-aware** `T_step_decode` form above —
> *not* the batch-size-1 `decode_capacity_tokens_per_sec` weight-only rate. The
> batch is KV-limited: `BatchSize = min(maxRunningReqs, TotalKVSlots / avgContext)`
> with `avgContext = L_in + L_out/2`. The batch-1 weight-only rate (the first
> formula) was found to be ~30× too conservative for batched serving — it vetoed
> Qwen2.5-14B/H100 at ~0.22 rps versus a real ~7 rps ceiling — so it is **not** used
> as the saturation bound. See `docs/example-runs/qwen2.5-14b-h100/`.

Where:
- `λ` = request arrival rate (req/s)
- `L_out` = mean output tokens per request
- `BW_peak_TB_s`, `TP` = hardware bandwidth and tensor-parallelism degree from `--gpu` / pool config
- `WeightBytesPerParam` = `EffectiveWeightBytesPerParam()` (quantization-aware; falls back to `BytesPerParam`)

#### Bottleneck B — KV cache capacity

Total KV slots: `C = NumKVBlocks × BlockSize` tokens. KV capacity bounds the running
batch (above): at most `C / avgContext` average-length contexts fit at once, where
`avgContext = L_in + L_out/2`. In the implemented pre-check this feeds `BatchSize`
rather than acting as a separate worst-case occupancy test.

A standalone KV saturation is reported only in the **degenerate** case where a single
average request's context does not fit at all:

```
if avgContext > C  →  KV-saturated (unservable) at any positive load
```

> An earlier implementation used a worst-case `maxConcurrency × (L_in + L_out)`
> occupancy here, which flagged saturation whenever the configured concurrency cap
> was high (e.g. 256) even at trivial load. Real engines dynamically cap the running
> batch at what KV allows and queue the rest, so that bound produced false positives;
> it was replaced by the batch-limiting form above.

### Where to find the parameters

| Parameter | Source |
|---|---|
| `BW_peak_TB_s` | `hwConfig.BwPeakTBs` (per-GPU, set by `--gpu` or pool `gpu_type`) |
| `TFlopsPeak` | `hwConfig.TFlopsPeak` |
| `NumParams`, `NumLayers`, `HiddenDim`, `NumKVHeads`, `HeadDim` | model config JSON (`model_configs/<model>/config.json`) |
| `WeightBytesPerParam` | `ModelConfig.EffectiveWeightBytesPerParam()` |
| `NumKVBlocks`, `BlockSize` | `KVCacheConfig` (set by `--kv-blocks`, `--block-size`) |
| `TP` | `--tp` flag or pool config |

### Code reference

- Weight + KV memory access formula: `sim/latency/roofline.go:159–229` (`calculateMemoryAccessBytes`)
- Trained-physics basis functions: `sim/latency/trained_physics_model.go:200–310`
- KV capacity in autoscaler: `sim/cluster/saturation_analyzer.go:83` (`k1 = TotalKvCapacityTokens × KvCacheThreshold`)
- Compute-bound capacity (`k2`) is currently a stub at `sim/cluster/saturation_analyzer.go:86` with the comment "future: derived from batch params" — the formula above is the intended implementation

---

## Method 2: Simulator output indicators

When a simulation is run, several output fields reliably signal overload regardless of
what the latency metrics report.

### Primary indicators (unambiguous)

| Field | JSON key | Signal |
|---|---|---|
| `NumStillQueued > 0` at run end | `still_queued` | Requests arrived faster than they could be served — definitive queue buildup |
| `KVAllocationFailures > 0` | `kv_allocation_failures` | KV cache hit capacity; memory-bound saturation |
| `NumTimedOut > 0` | `timed_out_requests` | Requests could not complete within SLO window |
| `NumDroppedUnservable > 0` | `dropped_unservable` | Hard rejection; server actively shedding load |

### Secondary indicators (corroborating)

| Signal | Interpretation |
|---|---|
| `responses_per_sec` plateaus while `e2e_mean_ms` grows | Classic saturated-queue behavior: throughput is capped, latency blows up |
| `still_queued / injected_requests` ratio increases with λ | The ratio locates the saturation point as load is swept |

### Recommended overload check

Evaluate in this order after each run:

1. **`still_queued / injected_requests > 0`** — if nonzero, the server was overloaded during
   the run horizon; latency metrics are unreliable.
2. **`kv_allocation_failures > 0`** — KV memory is the binding constraint; reduce sequence
   length, increase `--kv-blocks`, or reduce rate.
3. **`timed_out_requests > 0`** — soft overload; the server could not drain the queue within
   SLO limits even if it eventually would.

If all three are zero, the reported latency metrics are trustworthy for analysis.

### Output metrics location

All fields above are in `MetricsOutput` (`sim/metrics_utils.go:53–83`), printed to stdout
as JSON at `sim/metrics.go:132`. Cluster-level aggregates including per-SLO-class
breakdowns are in `sim/cluster/metrics.go:88–120`.

---

## Summary

| Goal | Approach | Key formula / field |
|---|---|---|
| Check overload before running | Batch-aware decode bandwidth | `λ × L_out > BatchSize / T_step_decode`, `BatchSize = min(maxRunningReqs, KVSlots/avgContext)` |
| Check overload before running | KV capacity (degenerate) | `avgContext > NumKVBlocks × BlockSize` (single request unservable) |
| Check overload after running | Queue buildup | `still_queued > 0` |
| Check overload after running | KV saturation | `kv_allocation_failures > 0` |
| Locate saturation point | Rate sweep | plot `still_queued / injected_requests` vs λ |
