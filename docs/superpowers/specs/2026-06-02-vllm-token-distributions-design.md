# vllm-server-evaluator: Configurable Token Length Distributions

**Date:** 2026-06-02
**Status:** Design
**Scope:** `vllm-server-evaluator/` only

## Motivation

The vllm-server-evaluator drives a Poisson stream of requests against a paired
real vLLM server. Today, every request in a measurement window uses the same
fixed `(InputTokens, OutputTokens)` pair derived from `ProblemData.AvgInputTokens`
/ `AvgOutputTokens`. Real production traffic has variability in both directions,
and that variability matters: output length variance drives KV-cache pressure
and decode tail latency, while input length variance affects prefill cost.

Replacing the constant token counts with configurable per-request samples lets
researchers exercise vLLM under more realistic, statistically richer workloads
without changing the request-arrival model (still Poisson) or the rest of the
pipeline.

## Proposal

Add two new optional fields to each `vllm-eval-config.json` entry:

```json
{
  "accelerator": "H100",
  "model": "ibm-granite/granite-3.1-8b-instruct",
  "...": "...",
  "inputTokenDistribution":  "fixed",
  "outputTokenDistribution": "geometric"
}
```

The averages remain in the request body (`AvgInputTokens`, `AvgOutputTokens`).
The config controls only the *shape* of the distribution around each average.
Input and output distributions are configured **independently** because their
underlying physics differs (prefill is one-shot; decode is sequential and
KV-bound) — researchers commonly want to vary one while holding the other fixed.

### Supported distribution types

All distributions take an integer `avg ≥ 1` and return integer samples `≥ 1`.

| Type | Support | Mean | Notes |
|------|---------|------|-------|
| `fixed` (default) | `{avg}` | `avg` | Current behavior. |
| `geometric` | `[1, 10·avg]` | ≈ `avg` | Truncated geometric, `p = 1/avg`. The 10× cap clips a vanishing tail (~e⁻¹⁰); empirical mean is essentially `avg`. |
| `uniform` | `[1, 2·avg − 1]` | `avg` | Discrete uniform. Wide spread; symmetric around `avg`. |
| `uniform-bounded` | `[max(1, ⌊avg/2⌋), ⌈3·avg/2⌉]` | ≈ `avg` | Tighter spread; ~symmetric around `avg`. Lower bound clamped to 1 so `avg = 1` still produces 1. |

Edge cases:
- `avg = 1` collapses every distribution to `{1}` — equivalent to `fixed`.
- Unknown distribution string: config-load fails fast at startup.
- The `10×` upper bound on `geometric` is a fixed implementation constant, not
  a config knob (YAGNI; can be promoted later if a real need appears).

### Sampling and reproducibility

Samples are drawn **per request** at arrival time inside `runWindow`. The
window's master seed (`wp.Seed`) is used to derive **three independent RNG
streams** — one each for arrivals, input-side draws, and output-side draws:

```
master := rand.New(rand.NewSource(wp.Seed))
arrivalsRNG := rand.New(rand.NewSource(master.Int63()))
inputRNG    := rand.New(rand.NewSource(master.Int63()))
outputRNG   := rand.New(rand.NewSource(master.Int63()))
```

- `arrivalsRNG` — Poisson interarrival gaps (`ExpFloat64()`).
- `inputRNG` — input token-count sample, plus the per-request seed used by
  `syntheticPromptTokens` for prompt content (both are input-side concerns).
- `outputRNG` — output token-count sample.

This is the **common random numbers (CRN)** idiom from simulation: two runs
with the same `wp.Seed` but different distribution config see the same
arrival times and the same input sequence, differing *only* where the
treatment differs. That isolates the effect under study from incidental
RNG-state shifts and makes A/B comparisons clean.

A single shared RNG would entangle these streams: changing the output
distribution would shift the RNG state cumulatively, perturbing every
subsequent arrival gap and input draw. With three streams that's impossible
by construction. The current code already uses one `*rand.Rand` for both
gaps and per-request seed generation, so this split also untangles a
pre-existing entanglement.

A fixed `wp.Seed` keeps the entire run deterministic.

## Architecture

### Affected files

- `vllm-server-evaluator/config.go` — add `inputTokenDistribution` /
  `outputTokenDistribution` to `configEntry` and `serverConfig`; validate
  values at load time.
- `vllm-server-evaluator/types.go` — `requestSpec` keeps its `InputTokens` /
  `OutputTokens` fields (they're the realized per-request values). No structural
  change.
- `vllm-server-evaluator/distribution.go` *(new)* — `tokenSampler` interface
  and the four concrete implementations. Pure functions of an `*rand.Rand`.
- `vllm-server-evaluator/generator.go` — `windowParams` carries an input
  sampler and an output sampler instead of a single fixed `Spec`. `runWindow`
  derives three RNGs from `wp.Seed` (arrivals / input / output) and uses them
  for the corresponding draws on each Poisson arrival.
- `vllm-server-evaluator/handler.go` — wires the configured distribution
  strings + averages into samplers and passes them into `windowParams`.

### New types (sketch)

```go
// distribution.go
type tokenSampler interface {
    Sample(*rand.Rand) int
}

type fixedSampler        struct{ avg int }
type geometricSampler    struct{ avg, hi int }
type uniformSampler      struct{ lo, hi int }
type uniformBoundedSampler struct{ lo, hi int }

func newSampler(kind string, avg int) (tokenSampler, error)
```

### Data flow

```
ProblemData (AvgInputTokens, AvgOutputTokens, RPS)
        │
        ▼
handler.go ── looks up serverConfig for (acc, model)
        │     reads inputTokenDistribution / outputTokenDistribution strings
        │     builds inputSampler  via newSampler(inDist,  AvgInputTokens)
        │     builds outputSampler via newSampler(outDist, AvgOutputTokens)
        ▼
windowParams { …, InputSampler, OutputSampler }   ← replaces Spec
        │
        ▼
runWindow ── derives arrivalsRNG / inputRNG / outputRNG from wp.Seed
          ── per Poisson arrival (gap from arrivalsRNG):
              promptSeed := inputRNG.Int63()
              spec := requestSpec{
                  InputTokens:  InputSampler.Sample(inputRNG),
                  OutputTokens: OutputSampler.Sample(outputRNG),
                  IgnoreEOS:    sc.IgnoreEOS,
              }
              go runOneRequest(..., spec, promptSeed)
```

`runOneRequest` is unchanged — it already takes a `requestSpec`.

### Backward compatibility

Both new fields are optional. Empty / missing → `"fixed"`. Existing config
files keep working unmodified and produce identical traffic to today. No
changes to the `/solve` API or to `ProblemData` / `AnalysisData`.

## Testing

New unit tests in `distribution_test.go`:
- For each non-fixed sampler: empirical mean over N=10 000 draws is within a
  small tolerance of `avg`.
- Bounds are respected: `min ≥ 1`, `max ≤ stated upper bound`.
- `avg = 1` returns 1 deterministically for all sampler kinds.
- `newSampler("bogus", 8)` returns an error.

Updates to existing tests:
- `config_test.go` — round-trip the two new fields; verify default-to-`"fixed"`.
- `handler_test.go` — confirm a non-`fixed` config produces variable token
  counts in the sampled requests (mock-friendly via the existing test seams).
- `generator_test.go` — `runWindow` accepts samplers; samples vary per request;
  with the same `wp.Seed`, two runs that differ *only* in `OutputSampler`
  produce identical arrival timings and identical input token sequences
  (CRN property check).

## Out of scope

- Other distributions (lognormal, Pareto, empirical from a trace file). Easy
  to add later behind the same `tokenSampler` interface if needed.
- Distribution parameters beyond `avg` (e.g. variance knobs). YAGNI.
- Per-request response logging / sample-level export.
- Changing the arrival process (still Poisson).
