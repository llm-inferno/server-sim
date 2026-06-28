# Configurable token-length distribution for blis-evaluator

- **Issue:** llm-inferno/server-sim#39
- **PR:** rescoped #38 (`feat/blis-bounded-token-dist`) closes #39
- **Date:** 2026-06-28
- **Status:** approved (brainstorming) — pending spec review

## Problem

PR #38 replaced the blis workload's unbounded exponential token-length
distribution with a clamped gaussian bounded by `MaxModelLen`. The fix works,
but the distribution choice and all of its parameters are **hardcoded** in
`tokenDist()` (`blis-evaluator/handler.go`): exponential when
`MaxModelLen <= 0`, otherwise a clamped gaussian with `std_dev = 0.5*mean`,
`min = 1`, and a sum-split `max`.

Every other blis simulation parameter is resolved per `(accelerator, model)`
entry from `blis-config.json`. The token-length distribution should be
configurable the same way rather than baked into the handler.

## Goal

Let each `blis-config.json` model entry choose its token-length distribution
**type** and parameters, while keeping the `MaxModelLen` bounding guarantee from
PR #38 intact for distributions that can be bounded.

Non-goals (YAGNI):

- Per-dimension distributions (separate input vs. output type). One spec applies
  to both dimensions, matching today's behavior.
- `empirical` and `pareto_lognormal` types. Not exposed.
- Threading additional distribution parameters through the `ProblemData`
  contract. The request still carries only means.

## Key constraint

`ProblemData` carries only **means** (`AvgInputTokens`, `AvgOutputTokens`). So:

- The `mean` of each dimension is always dynamic (per request), never config.
- The clamp `max` is derived from `MaxModelLen` via the existing sum-split, also
  dynamic.

Config therefore supplies only the *remaining*, mean-relative knobs. We express
those as **curated semantic knobs** — not raw library `DistSpec` params — so the
evaluator owns the math and configs cannot express values that conflict with the
dynamic mean/max.

## Config schema

New optional `tokenDist` block on `modelEntry` (`blis-evaluator/config.go`):

```jsonc
"tokenDist": {
  "type": "gaussian",   // "constant" | "exponential" | "gaussian" | "lognormal"
  "cov": 0.5,            // coefficient of variation (std_dev/mean); gaussian & lognormal only
  "min": 1               // clamp floor (default 1)
}
```

```go
type tokenDistConfig struct {
    Type string  `json:"type"` // "constant" | "exponential" | "gaussian" | "lognormal"
    Cov  float64 `json:"cov"`  // coefficient of variation (std_dev/mean); gaussian & lognormal
    Min  float64 `json:"min"`  // clamp floor; default 1
}

type modelEntry struct {
    // ... existing fields ...
    TokenDist *tokenDistConfig `json:"tokenDist"` // nil → constant (fixed-length) default
}
```

`cov` (coefficient of variation = `std_dev / mean`) is a single spread knob that
generalizes across both spread-bearing types, replacing a gaussian-only
`std_dev` fraction and removing any need for a separate lognormal `sigma` field.

## Type → DistSpec translation

The evaluator synthesizes the library `DistSpec` from the dynamic `mean`, the
sum-split `max` (when `MaxModelLen > 0`), and the config knobs:

| Type          | `mean`        | spread                                  | clamp                         | `MaxModelLen > 0` |
|---------------|---------------|-----------------------------------------|-------------------------------|-------------------|
| `constant` (default) | `value = round(mean)` | none (deterministic)            | n/a — no tail                 | allowed           |
| `exponential` | from request  | CoV fixed at 1 (`cov` ignored)          | none (sampler ignores min/max)| **rejected at load** |
| `gaussian`    | from request  | `std_dev = cov * mean`                  | `min` from config, `max` = sum-split share | allowed |
| `lognormal`   | from request  | `sigma = sqrt(ln(1 + cov^2))`, `mu = ln(mean) - sigma^2/2` | `min`/sum-split `max` | allowed |

Lognormal `mu` is derived so that the lognormal's expected value equals the
request mean (`E[X] = exp(mu + sigma^2/2) = mean`).

The sum-split `max` is unchanged from PR #38:

```
max = MaxModelLen * mean / (mean + otherMean)   (>= 1)
```

applied to each clampable dimension so `input + output <= MaxModelLen` holds by
construction. `constant` needs no clamp: its deterministic sequence length is
`inMean + outMean`, within `MaxModelLen` whenever the means themselves are
(a workload precondition outside the distribution's control).

## Default behavior change

Today, an entry with no `tokenDist` block gets PR #38's clamped-gaussian
(bounded) or exponential (unbounded). After this change, **absent `tokenDist`
defaults to `constant`** (fixed-length, zero variance).

To avoid silently changing existing simulation behavior, every current
`blis-config.json` entry gains an explicit `tokenDist` block preserving its
present behavior (chosen approach (b) during brainstorming):

- `Qwen/Qwen2.5-14B-Instruct` / H100 (`maxModelLen = 4096`):
  `{"type": "gaussian", "cov": 0.5, "min": 1}`
- All other 10 entries (`maxModelLen = 0`): `{"type": "exponential"}`

## Validation (fail loud at config load)

Extend `validateEntry` (runs before defaults; any error fails the whole load,
matching the existing blis convention of validating `> 0` fields up front):

1. If `TokenDist != nil`:
   - `type` must be one of `constant`, `exponential`, `gaussian`, `lognormal`.
   - If `MaxModelLen > 0` and `type == "exponential"` → reject. Exponential is
     the only unclampable type in the set; its sampler ignores `min`/`max`, so
     the bound cannot be enforced. (Constant is deterministic; gaussian and
     lognormal clamp.)
   - `gaussian` / `lognormal`: `cov > 0` required.
   - `min`, if set, must be `>= 1`.
   - Stray `cov`/`min` on `constant`/`exponential`: **ignored with a logged
     warning** (lenient — keeps configs forward-compatible without hard
     failures on harmless extras).

`applyDefaults` sets `Min = 1` when a `tokenDist` block is present with `min`
unset/zero.

## Handler integration

`tokenDist()` in `handler.go` takes the entry's `*tokenDistConfig` and switches
on `Type`:

- `nil` → `constant{value: round(mean)}`.
- `constant` → `constant{value: round(mean)}`.
- `exponential` → `exponential{mean}` (only reachable when `MaxModelLen <= 0`,
  guaranteed by config validation).
- `gaussian` → `gaussian{mean, std_dev: cov*mean, min, max: sum-split}`.
- `lognormal` → `lognormal{mu, sigma, min, max: sum-split}` with `mu`/`sigma`
  derived as above.

The sum-split computation moves into this function unchanged. The two call sites
(`InputDist`, `OutputDist`) pass the same `*tokenDistConfig` with `mean` and
`otherMean` swapped, exactly as today.

## Testing

Extend `blis-evaluator/tokendist_test.go`:

- **Per-type synthesis**: gaussian `std_dev = cov*mean`; lognormal `mu`/`sigma`
  derivation reproduces the target mean within tolerance (sampled-mean check);
  constant `value = round(mean)`; `nil` config → constant.
- **Bounding**: sum-split `max` holds for gaussian *and* lognormal; sampled
  values stay within `[min, max]`; `input + output <= MaxModelLen` over many
  samples (extend the existing run19-style test).
- **Validation** (table tests against `validateEntry`): exponential +
  `MaxModelLen > 0` rejected; unknown type rejected; `cov <= 0` for
  gaussian/lognormal rejected; `min < 1` rejected; valid blocks accepted.

## Docs

- Update `docs/blis-evaluator-config.md` with the `tokenDist` block, the type
  table, the `constant` default, and the `MaxModelLen > 0` rejection rule.
- Update the migrated `blis-config.json` entries.

## Out of scope / follow-ups

- Per-dimension distributions.
- `empirical` / `pareto_lognormal` support.
