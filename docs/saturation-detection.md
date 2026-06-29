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

- **BLIS pre-sim** (DES was skipped): latency metrics are populated with a large,
  load-monotonic *saturated* latency derived from the decode-bandwidth model (not zero), so
  the value is monotonic with load and renders distinctly from a "no data" dropout. See
  `docs/blis-saturated-latency.md` (issue #40).
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

A single **batch-aware** analytical check runs **before** the DES to avoid expensive
simulations on overloaded configurations. A real vLLM server decodes a whole running batch
per forward pass — streaming the model weights once and reading the KV of every in-flight
context — so the running batch `B` is bounded by KV capacity:

```
avgContext = L_in + L_out/2
B          = min(maxRunningReqs, TotalKVSlots / avgContext)
```

The decode token-throughput ceiling at that batch is then:

```
t_step    = (weightBytes + B × avgContext × kvBytesPerToken) / (BW × TP)
decodeTPS = B / t_step
```

1. **Decode bandwidth:** `RPS × AvgOutputTokens > decodeTPS × saturationMargin`
   → `"bandwidth"`, `MaxRPS = decodeTPS / AvgOutputTokens`.
2. **KV cache (degenerate):** a single average request's context does not fit at all
   (`B_kv < 1`, i.e. `avgContext > TotalKVSlots`) → `"kv_capacity"`, `MaxRPS = 0`.

The earlier form compared aggregate demand against a batch-size-1 weight-streaming rate and
used a worst-case `maxConcurrency × tokens` KV occupancy; both ignored batching and were far
too conservative for batched serving (e.g. they vetoed Qwen2.5-14B/H100 at ~0.22 rps versus a
real ~7 rps ceiling). See `docs/blis-overload-detection.md` for the derivation.

If the pre-check does not fire and the DES runs, a post-sim check inspects the output for
`StillQueued > 0`, `KVAllocationFailures > 0`, or `TimedOutRequests > 0` — any of these sets
`"overloaded"` while leaving the DES-computed metrics in place.

### vllm-server

Three runtime signals are evaluated over the measurement window; any one triggers `"overloaded"`:

1. **TTFT trend:** linear-regression slope indicates >50% TTFT growth from start to end of window.
2. **Queue dominance:** scraped `vllm:request_queue_time_seconds` mean exceeds
   `vllm:request_inference_time_seconds` mean.
3. **Error rate:** ≥5% request errors, or any `429` from vLLM.
