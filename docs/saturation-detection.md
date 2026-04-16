# Saturation Detection for server-sim Evaluators

## Context

When a simulated LLM server is overloaded, the reported latency metrics (TTFT, ITL, E2E) are unreliable — queues grow unboundedly and averages reflect queue buildup rather than intrinsic serving latency. Additionally, BLIS DES simulations on overloaded configurations are expensive (minutes of wall-clock time) and produce meaningless results.

The goal is to detect saturation at the evaluator level so that:
1. BLIS avoids running the expensive DES on overloaded configurations (analytical pre-check)
2. Queue-analysis explicitly flags when RPS exceeds analytical MaxRate
3. Consumers receive a clear signal that metrics are unreliable
4. server-sim skips noise injection for saturated results

See also: `docs/blis-overload-detection.md` for the underlying analytical formulas.

---

## Approach: `Saturation` string field on AnalysisData

Add a `Saturation` field (string) to `AnalysisData`. Empty string = not saturated. Non-empty = saturated with a reason.

**Why a string instead of a boolean**: The bottleneck type is operationally actionable — bandwidth saturation suggests adding GPUs/upgrading hardware, while KV saturation suggests reducing sequence length or increasing `--kv-blocks`. A string field with `omitempty` is also backward-compatible: absent from JSON when empty, so existing consumers are unaffected.

### Saturation values

| Value | Meaning | Typical remediation |
|-------|---------|---------------------|
| `""` (absent) | Not saturated; metrics are reliable | — |
| `"bandwidth"` | Decode memory bandwidth is the bottleneck | Add GPUs, reduce TP, use quantization |
| `"kv_capacity"` | KV cache capacity is exhausted | Increase `--kv-blocks`, reduce sequence length |
| `"overloaded"` | Generic overload (queue-analysis or post-sim DES indicators) | Reduce RPS or scale replicas |

### Tolerance margin

All saturation checks apply a 2% headroom (`saturationMargin = 0.98`) to account for estimation inaccuracy in the analytical formulas:
```
demand > capacity * saturationMargin   →  saturated
```
This avoids false positives from rounding and approximate parameter counts (especially for MoE models).

### Metric contract when saturated

The `Saturation` field is the **authoritative signal**. Metrics in a saturated response are left as-is:
- **BLIS pre-sim** (DES was skipped): all latency metrics are zero by construction.
- **Queue-analysis and BLIS post-sim**: metrics are populated with degraded-state values. Consumers that care about reliability MUST check `Saturation` before interpreting them.
- **MaxRPS** is populated where it can be computed, including BLIS (derived from the bandwidth ceiling).
- **Noise is not applied** to saturated results.

---

## Implementation

### 1. `pkg/evaluator/types.go` — extend AnalysisData

```go
const (
    SaturationNone      = ""
    SaturationBandwidth = "bandwidth"
    SaturationKV        = "kv_capacity"
    SaturationOverload  = "overloaded"
)

type AnalysisData struct {
    Throughput  float32 `json:"throughput"`
    AvgRespTime float32 `json:"avgRespTime"`
    AvgWaitTime float32 `json:"avgWaitTime"`
    AvgTTFT     float32 `json:"avgTTFT"`
    AvgITL      float32 `json:"avgITL"`
    MaxRPS      float32 `json:"maxRPS"`
    Saturation  string  `json:"saturation,omitempty"`
}

func (ad AnalysisData) IsSaturated() bool {
    return ad.Saturation != ""
}
```

### 2. `blis-evaluator/handler.go` — analytical pre-simulation check

A `checkSaturation` function runs **before** the DES using parameters already loaded in the handler. It returns the saturation reason and a computed MaxRPS.

#### A. Decode bandwidth bound

Based on `docs/blis-overload-detection.md` Method 1, Bottleneck A:

```
totalWeightBytes  = estimateModelParams(mc) * mc.EffectiveWeightBytesPerParam()
decodeCapacityTPS = (hwConfig.BwPeakTBs * TP * 1e12) / (totalWeightBytes / TP)
Saturated if:      RPS * AvgOutputTokens > decodeCapacityTPS * saturationMargin
MaxRPS (derived):  decodeCapacityTPS / AvgOutputTokens
```

`estimateModelParams` replicates the essential formula from the unexported `computeModelWeightBytes` in inference-sim (embeddings + attention projections + MLP matrices + norms). For MoE models it uses total params across all experts, which overestimates weight traffic — a conservative, safe choice for a pre-sim gate.

#### B. KV cache capacity bound

Simplified from Bottleneck B (avoids T_e2e circularity):

```
totalKVSlots       = TotalKVBlocks * BlockSizeTokens
avgTokensPerReq    = AvgInputTokens + AvgOutputTokens
concurrentKVTokens = MaxRunningReqs * avgTokensPerReq
Saturated if:       concurrentKVTokens > totalKVSlots * saturationMargin
```

This is a necessary condition: if MaxRunningReqs requests at average sequence length cannot fit in KV cache, the system is definitely KV-saturated.

#### Insertion point in solveHandler

```go
// Between hwConfig loading and workload spec construction:
if sat, maxRPS := checkSaturation(pd, modelConfig, hwConfig, entry); sat != "" {
    c.IndentedJSON(http.StatusOK, evaluator.AnalysisData{Saturation: sat, MaxRPS: maxRPS})
    return
}
// ... proceed with DES simulation
```

#### Post-sim safety net (optional, lower priority)

After the DES completes, inspect `cs.AggregatedMetrics()` for overload indicators (e.g., `StillQueued > 0`, `KVAllocationFailures > 0`). If detected, set `Saturation = "overloaded"` on the AnalysisData but leave the DES-computed metrics in place — they represent actual degraded-state behavior. The exact field names on `*sim.Metrics` need verification against the inference-sim v0.7.4 API.

### 3. `queue-analysis-evaluator/handler.go` — RPS vs MaxRate

After `qa.Analyze(pd.RPS)` succeeds:

```go
ad := evaluator.AnalysisData{
    Throughput: metrics.Throughput, ...
    MaxRPS: metrics.MaxRate,
}
if float64(pd.RPS) > float64(metrics.MaxRate)*saturationMargin {
    ad.Saturation = evaluator.SaturationOverload
    // Leave metrics populated — degraded-state values.
    // MaxRPS remains set so consumers know the capacity limit.
}
```

### 4. `pkg/server/server.go` — skip noise when saturated

```go
if !result.IsSaturated() && s.cfg.NoiseEnabled {
    result = noise.AddNoise(result, s.cfg.Noise)
    if result.Throughput > pd.RPS {
        result.Throughput = pd.RPS
    }
}
```

This also fulfils the documented-but-previously-unimplemented invariant in CLAUDE.md ("maxRPS = 0 from evaluator means evaluator is overloaded; skip noise").

### 5. `pkg/noise/noise.go` — preserve Saturation field

The `AddNoise` return struct must include `Saturation: ad.Saturation` to avoid silently dropping the flag if noise is ever applied to a saturated result.

### 6. `dummy-evaluator/main.go` — flag saturation when RPS > MaxRPS

```go
if ad.MaxRPS > 0 && float64(pd.RPS) > float64(ad.MaxRPS)*saturationMargin {
    ad.Saturation = evaluator.SaturationOverload
}
```

---

## Files modified

| File | Change |
|------|--------|
| `pkg/evaluator/types.go` | `Saturation` field, constants, `IsSaturated()` |
| `blis-evaluator/handler.go` | `checkSaturation`, `estimateModelParams`; DES gate |
| `queue-analysis-evaluator/handler.go` | RPS > MaxRate check |
| `pkg/server/server.go` | Skip noise when `IsSaturated()` |
| `pkg/noise/noise.go` | Preserve `Saturation` in `AddNoise` |
| `dummy-evaluator/main.go` | RPS > MaxRPS saturation flag |
| `CLAUDE.md` | Document new field and invariants |

---

## Verification

1. `go build ./...` — all packages compile.
2. **BLIS bandwidth saturation**: send RPS high enough that `RPS * AvgOutputTokens > bandwidth_ceiling * 0.98`. Expect `saturation: "bandwidth"`, `maxRPS` populated, latency metrics zero (DES skipped).
3. **BLIS KV saturation**: send a config where `MaxRunningReqs * avgSeqLen > TotalKVBlocks * BlockSize * 0.98`. Expect `saturation: "kv_capacity"`.
4. **Queue-analysis overload**: send RPS > MaxRate. Expect `saturation: "overloaded"`, metrics still populated, `maxRPS` set.
5. **Noise skip**: server-sim with `NOISE_ENABLED=true` — saturated result should pass through unchanged.
6. **Normal path**: sub-saturation RPS — all evaluators return normal metrics, no `saturation` field in JSON.
7. **Boundary**: RPS at ~98–102% of capacity — verify the tolerance margin fires correctly.
