# Saturation Detection

When a simulated LLM server is overloaded, reported latency metrics (TTFT, ITL, E2E) are
unreliable — queues grow unboundedly and averages reflect queue buildup rather than intrinsic
serving latency. Each evaluator detects saturation and sets the `Saturation` field on
`AnalysisData` so that consumers can distinguish reliable metrics from degraded-state readings.

See also: `docs/blis-overload-detection.md` for the underlying BLIS analytical formulas.

---

## Saturation values

| Value | Meaning | Typical remediation |
|-------|---------|---------------------|
| `""` (absent) | Not saturated; metrics are reliable | — |
| `"bandwidth"` | Decode memory bandwidth is the bottleneck | Add GPUs, reduce TP, use quantization |
| `"kv_capacity"` | KV cache capacity is exhausted | Increase KV blocks, reduce sequence length |
| `"overloaded"` | Generic overload (queue-analysis, post-sim DES indicators, or vllm-server runtime signals) | Reduce RPS or scale replicas |

## Tolerance margin

All analytical saturation checks apply a 2% headroom (`saturationMargin = 0.98`):

```
demand > capacity × saturationMargin  →  saturated
```

This avoids false positives from rounding and approximate parameter counts (especially for
MoE models).

## Metric contract when saturated

The `Saturation` field is the **authoritative signal**. Metrics in a saturated response are
left as-is by the evaluator:

- **BLIS pre-sim** (DES was skipped): all latency metrics are zero by construction.
- **Queue-analysis and BLIS post-sim**: metrics are populated with degraded-state values.
- **vllm-server**: metrics reflect the measured degraded behaviour under the offered load.
- **`MaxRPS`** is populated where it can be computed: BLIS derives it from the bandwidth
  ceiling; queue-analysis sets it from the analytical `MaxRate`; vllm-server leaves it 0
  (no capacity sweep is performed).
- **Noise is never applied** to saturated results.

Consumers MUST check `Saturation` before interpreting latency metrics.

---

## Per-evaluator behaviour

### dummy

Flags `"overloaded"` when `RPS > MaxRPS` (the hardcoded capacity limit).

### queue-analysis

Flags `"overloaded"` when `RPS > MaxRate × saturationMargin`. Metrics remain populated with
degraded-state analytical values; `MaxRPS` is set to `MaxRate`.

### blis

Two independent analytical checks run **before** the DES to avoid expensive simulations on
overloaded configurations:

1. **Bandwidth (Bottleneck A):** `RPS × AvgOutputTokens > decodeCapacityTPS × saturationMargin`
   → `"bandwidth"`, `MaxRPS` derived from the bandwidth ceiling.
2. **KV cache (Bottleneck B):** `MaxRunningReqs × avgSeqLen > TotalKVBlocks × BlockSize × saturationMargin`
   → `"kv_capacity"`.

If neither pre-check fires and the DES runs, a post-sim check inspects the output for
`StillQueued > 0`, `KVAllocationFailures > 0`, or `TimedOutRequests > 0` — any of these sets
`"overloaded"` while leaving the DES-computed metrics in place.

### vllm-server

Three runtime signals are evaluated over the measurement window; any one triggers `"overloaded"`:

1. **TTFT trend:** linear-regression slope indicates >50% TTFT growth from start to end of window.
2. **Queue dominance:** scraped `vllm:request_queue_time_seconds` mean exceeds
   `vllm:request_inference_time_seconds` mean.
3. **Error rate:** ≥5% request errors, or any `429` from vLLM.
