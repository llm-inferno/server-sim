# BLIS Saturated-Latency Reporting

Tracks [issue #40](https://github.com/llm-inferno/server-sim/issues/40).

## Problem

When the BLIS evaluator's analytical **pre-check** (`checkSaturation`) trips, it skips the
expensive DES and returns immediately. Until now that early return populated only the
`Saturation` reason and `MaxRPS`, leaving every latency field at its zero value:

```go
c.IndentedJSON(http.StatusOK, evaluator.AnalysisData{
    Saturation: sat,
    MaxRPS:     maxRPS,
})   // AvgITL, AvgTTFT, AvgRespTime, AvgWaitTime, Throughput all 0
```

Downstream, the control-loop collector forwards `AvgITL`/`AvgTTFT` verbatim into the
deployment's attained latencies, and the dashboard plots the zeros as a **0 ms dropout** —
visually indistinguishable from a missing/failed reading, when in fact the pod is overloaded
(the *opposite* of low latency).

A zero latency is semantically wrong for an overloaded server. This is observability-only: it
does **not** affect autoscaling, which keys off ITL/throughput agreed by both backends.

## Direction (chosen)

Of the two directions in #40, this implements **Direction A**: compute and return a **large,
load-monotonic** saturated ITL/TTFT from the pre-check's own intermediates, so the value is
honest (high latency for an overloaded pod) and the dashboard renders it as saturation rather
than a dropout.

This is a **server-sim-only** change. No wire-contract change: the `/latest` envelope already
carries `saturation` (the collector already decodes it), and the collector already forwards
`AvgITL`/`AvgTTFT` — so non-zero values flow through with no control-loop edit. The
complementary EKF-side gating of saturated samples is tracked separately in control-loop#24 and
keys off the `saturation` flag that already propagates.

## Saturated-latency model

The pre-check already derives, for the KV-limited running batch `B`:

```
avgContext = L_in + L_out/2                              (mean KV tokens per in-flight request)
tStep      = (weightBytes + B*avgContext*kvBytesPerToken) / (BW*TP)   (decode step time, s)
decodeTPS  = B / tStep
maxRPS     = decodeTPS / L_out
```

From these we report:

| Field | Formula | Rationale |
|-------|---------|-----------|
| `AvgITL` | `tStep` (→ ms) | Each running request emits one token per decode step, so the per-token latency of the saturated batch **is** `tStep`. Plateaus with load (batch is capped) — physically correct — but large vs. lightly-loaded ITL and monotonic in context length. |
| `AvgTTFT` | `genMs × overload` | A newly arriving request waits ~one full generation time (`genMs = L_out × ITL`) for a slot to free at the saturation boundary; the wait scales with the overload factor as load climbs. |
| `AvgWaitTime` | `AvgTTFT` | Under saturation the queueing wait dominates first-token time; we have no separate prefill estimate in the pre-check. |
| `AvgRespTime` | `AvgTTFT + genMs` | wait + generation. Keeps `RespTime − WaitTime = genMs` (in-service time), so the collector's Little's-Law occupancy `Throughput × (RespTime−WaitTime)` ≈ `B` stays physically sane. |
| `Throughput` | `maxRPS` | Saturated goodput ceiling. `maxRPS < RPS` under saturation, so the `Throughput ≤ RPS` invariant holds. |
| `MaxRPS` | `maxRPS` | Unchanged. |

where

```
genMs    = L_out × AvgITL
overload = max(1, RPS / maxRPS)          # = RPS / maxRPS once past the ceiling
```

**Load-monotonicity.** `overload = RPS / maxRPS` grows linearly once `RPS` exceeds the
capacity ceiling, so `AvgTTFT` (and `AvgRespTime`) rise monotonically with offered load — the
dashboard shows a climbing latency curve under a load ramp, the desired distinct-from-dropout
signal. `AvgITL` is flat at `tStep` (a saturated batch decodes at a fixed step time regardless
of how much load is queued behind it), which is the physically correct behaviour.

## KV-degenerate case (`B_kv < 1`)

When a single average request's context does not fit in KV at all, there is no stable serving
rate (`maxRPS = 0`). We still emit a non-zero latency rather than a misleading zero, computed
from the same model at `B = 1` with `overload = 1` (no stable rate to form a ratio). At the
deployment's real (finite) bandwidth this is a large value; the `kv_capacity` flag remains the
authoritative "unservable" signal. This branch is a pathological *config-level* impossibility
(distinct from the transient bandwidth over-subscription that motivated #40), so the latency is
best-effort and documented as approximate.

## Non-goals

- Queueing-theoretic exactness. Under true saturation steady-state latency is unbounded; we
  report a large, finite, monotonic proxy for observability, not a calibrated prediction.
- Changes to the post-sim safety-net path (DES ran, overload indicators present), which already
  carries real degraded-state metrics.
- Any control-loop change. EKF-side gating of saturated samples is control-loop#24.
