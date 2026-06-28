# Configurable blis token-length distribution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the blis-evaluator token-length distribution configurable per `blis-config.json` model entry (type + coefficient of variation + min), replacing the hardcoded `tokenDist()` from PR #38.

**Architecture:** Add an optional `tokenDist` block to `modelEntry`. The request supplies the dynamic `mean` (`ProblemData` carries only means); config supplies `cov`/`min`; the `MaxModelLen` sum-split supplies the clamp `max`. `tokenDist()` switches on the configured type and synthesizes the library `workload.DistSpec`. A `nil` block defaults to `constant` (fixed-length). Unclampable `exponential` is rejected at config load when `MaxModelLen > 0`.

**Tech Stack:** Go, `github.com/inference-sim/inference-sim/sim/workload`, standard `testing`.

## Global Constraints

- Module: `github.com/llm-inferno/server-sim`. All work is in `blis-evaluator/`.
- Spec: `docs/superpowers/specs/2026-06-28-blis-configurable-token-dist-design.md`. Tracking issue: #39. Work folds into open PR #38 on branch `feat/blis-bounded-token-dist`.
- Supported types: `constant`, `exponential`, `gaussian`, `lognormal` (no `empirical`/`pareto_lognormal`).
- One spec applies to both input and output dimensions (mean/otherMean swapped), matching current behavior.
- `cov` = coefficient of variation = `std_dev / mean`. Gaussian: `std_dev = cov*mean`. Lognormal: `sigma = sqrt(ln(1+cov²))`, `mu = ln(mean) − sigma²/2`.
- Sum-split unchanged: `max = MaxModelLen * mean / (mean + otherMean)`, floored at 1.
- `min` default = 1, applied only when a `tokenDist` block is present.
- `MaxModelLen > 0` + `type == "exponential"` → reject at config load (fail loud, matches blis convention).
- Run all tests from repo root: `go test ./blis-evaluator/...`.

---

### Task 1: Config schema, validation, and defaults

**Files:**
- Modify: `blis-evaluator/config.go` (add `tokenDistConfig` type + `TokenDist` field; extend `validateEntry` and `applyDefaults`; add `validateTokenDist`; add `log` import)
- Test: `blis-evaluator/config_test.go` (new)

**Interfaces:**
- Produces: `type tokenDistConfig struct { Type string; Cov float64; Min float64 }` with JSON tags `type`/`cov`/`min`; new field `modelEntry.TokenDist *tokenDistConfig` (JSON `tokenDist`). `func validateTokenDist(td *tokenDistConfig, maxModelLen int64) error`.
- Consumes: existing `modelEntry`, `validateEntry`, `applyDefaults` in `config.go`.

- [ ] **Step 1: Write the failing test**

Create `blis-evaluator/config_test.go`:

```go
package main

import "testing"

// validBase returns a minimal modelEntry that passes validateEntry's required-field
// checks, so token-dist cases test only the tokenDist rules.
func validBase() modelEntry {
	return modelEntry{
		Accelerator:        "H100",
		Model:              "test/model",
		HFConfigPath:       "hf-configs/test/config.json",
		GPU:                "H100",
		TotalKVBlocks:      1000,
		MaxRunningReqs:     256,
		MaxScheduledTokens: 8192,
	}
}

func TestValidateEntry_TokenDist(t *testing.T) {
	cases := []struct {
		name        string
		maxModelLen int64
		td          *tokenDistConfig
		wantErr     bool
	}{
		{"nil block ok", 4096, nil, false},
		{"constant ok bounded", 4096, &tokenDistConfig{Type: "constant"}, false},
		{"gaussian ok bounded", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 1}, false},
		{"lognormal ok bounded", 4096, &tokenDistConfig{Type: "lognormal", Cov: 0.5}, false},
		{"exponential ok unbounded", 0, &tokenDistConfig{Type: "exponential"}, false},
		{"exponential rejected bounded", 4096, &tokenDistConfig{Type: "exponential"}, true},
		{"unknown type rejected", 0, &tokenDistConfig{Type: "weibull"}, true},
		{"gaussian cov<=0 rejected", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0}, true},
		{"lognormal cov<=0 rejected", 4096, &tokenDistConfig{Type: "lognormal", Cov: -1}, true},
		{"negative min rejected", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: -2}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validBase()
			m.MaxModelLen = tc.maxModelLen
			m.TokenDist = tc.td
			err := validateEntry(&m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateEntry err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestApplyDefaults_TokenDistMin(t *testing.T) {
	m := validBase()
	m.TokenDist = &tokenDistConfig{Type: "gaussian", Cov: 0.5} // Min unset → 0
	applyDefaults(&m)
	if m.TokenDist.Min != 1 {
		t.Fatalf("TokenDist.Min = %v, want default 1", m.TokenDist.Min)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./blis-evaluator/ -run 'TestValidateEntry_TokenDist|TestApplyDefaults_TokenDistMin' -v`
Expected: compile error — `tokenDistConfig` undefined and `modelEntry` has no field `TokenDist`.

- [ ] **Step 3: Add the type, field, validation, and defaults**

In `blis-evaluator/config.go`, add `"log"` to the import block (alongside `encoding/json`, `fmt`, `os`).

Add the new type just above `// blisConfig is the top-level structure...`:

```go
// tokenDistConfig configures the token-length distribution synthesized for the
// blis workload. The request supplies the mean (ProblemData carries only means);
// cov and min come from config; the clamp max is derived from MaxModelLen
// (sum-split). A nil block means the constant (fixed-length) default.
type tokenDistConfig struct {
	Type string  `json:"type"` // "constant" | "exponential" | "gaussian" | "lognormal"
	Cov  float64 `json:"cov"`  // coefficient of variation (std_dev/mean); gaussian & lognormal only
	Min  float64 `json:"min"`  // clamp floor; default 1
}
```

Add the `TokenDist` field to `modelEntry` (after `Seed`):

```go
	Seed               int64     `json:"seed"`               // RNG seed for deterministic results
	TokenDist          *tokenDistConfig `json:"tokenDist"`   // optional; nil → constant (fixed-length) default
```

In `validateEntry`, before the final `return nil`, add:

```go
	if m.TokenDist != nil {
		if err := validateTokenDist(m.TokenDist, m.MaxModelLen); err != nil {
			return err
		}
	}
```

Add the helper after `validateEntry`:

```go
// validateTokenDist checks the optional token-distribution block. cov/min are
// ignored (with a warning) for types that don't use them. Exponential cannot be
// bounded — its sampler ignores min/max — so it is rejected when MaxModelLen > 0.
func validateTokenDist(td *tokenDistConfig, maxModelLen int64) error {
	switch td.Type {
	case "constant", "exponential", "gaussian", "lognormal":
	default:
		return fmt.Errorf("tokenDist.type %q must be one of constant, exponential, gaussian, lognormal", td.Type)
	}
	if maxModelLen > 0 && td.Type == "exponential" {
		return fmt.Errorf("tokenDist.type %q cannot be bounded by maxModelLen (%d); use gaussian or lognormal", td.Type, maxModelLen)
	}
	if (td.Type == "gaussian" || td.Type == "lognormal") && td.Cov <= 0 {
		return fmt.Errorf("tokenDist.cov must be > 0 for type %q", td.Type)
	}
	if td.Min < 0 {
		return fmt.Errorf("tokenDist.min must be >= 0, got %v", td.Min)
	}
	if (td.Type == "constant" || td.Type == "exponential") && (td.Cov != 0 || td.Min != 0) {
		log.Printf("blis config: tokenDist.cov/min ignored for type %q", td.Type)
	}
	return nil
}
```

In `applyDefaults`, before the final closing brace, add:

```go
	if m.TokenDist != nil && m.TokenDist.Min <= 0 {
		m.TokenDist.Min = 1
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./blis-evaluator/ -run 'TestValidateEntry_TokenDist|TestApplyDefaults_TokenDistMin' -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Verify the whole package still builds and tests pass**

Run: `go build ./... && go test ./blis-evaluator/...`
Expected: build succeeds; existing `tokendist_test.go` and `saturation_test.go` still pass (handler still uses the old `tokenDist` signature — unchanged in this task).

- [ ] **Step 6: Commit**

```bash
git add blis-evaluator/config.go blis-evaluator/config_test.go
git commit -m "feat(blis): add per-entry tokenDist config schema and validation"
```

---

### Task 2: Config-driven `tokenDist()` synthesis

**Files:**
- Modify: `blis-evaluator/handler.go` (rewrite `tokenDist`; add `sumSplitMax`; update the two call sites; add `math` import)
- Test: `blis-evaluator/tokendist_test.go` (rewrite for the new signature)

**Interfaces:**
- Consumes: `tokenDistConfig` from Task 1; `workload.DistSpec`, `workload.NewLengthSampler` from the library.
- Produces: `func tokenDist(cfg *tokenDistConfig, mean, otherMean, maxModelLen float64) workload.DistSpec`; `func sumSplitMax(mean, otherMean, maxModelLen float64) float64`.

- [ ] **Step 1: Rewrite the test for the new signature**

Replace the entire contents of `blis-evaluator/tokendist_test.go` with:

```go
package main

import (
	"math"
	"math/rand"
	"testing"

	"github.com/inference-sim/inference-sim/sim/workload"
)

// nil config and explicit "constant" both produce a deterministic value = round(mean).
func TestTokenDist_ConstantDefault(t *testing.T) {
	for _, cfg := range []*tokenDistConfig{nil, {Type: "constant"}} {
		d := tokenDist(cfg, 1024, 512, 4096)
		if d.Type != "constant" {
			t.Fatalf("type = %q, want constant (cfg=%v)", d.Type, cfg)
		}
		if d.Params["value"] != 1024 {
			t.Fatalf("value = %v, want 1024 (cfg=%v)", d.Params["value"], cfg)
		}
	}
}

// Explicit exponential (only legal unbounded) carries the mean only.
func TestTokenDist_Exponential(t *testing.T) {
	d := tokenDist(&tokenDistConfig{Type: "exponential"}, 1024, 512, 0)
	if d.Type != "exponential" {
		t.Fatalf("type = %q, want exponential", d.Type)
	}
	if d.Params["mean"] != 1024 {
		t.Fatalf("mean = %v, want 1024", d.Params["mean"])
	}
}

// Gaussian: std_dev = cov*mean, min from config, and the input/output caps
// sum-split MaxModelLen so input+output budgets never exceed it.
func TestTokenDist_GaussianParamsAndSumSplit(t *testing.T) {
	cfg := &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 1}
	in := tokenDist(cfg, 1024, 512, 4096)
	out := tokenDist(cfg, 512, 1024, 4096)

	if in.Type != "gaussian" || out.Type != "gaussian" {
		t.Fatalf("type = %q/%q, want gaussian", in.Type, out.Type)
	}
	if in.Params["mean"] != 1024 || out.Params["mean"] != 512 {
		t.Fatalf("means not preserved: in=%v out=%v", in.Params["mean"], out.Params["mean"])
	}
	if in.Params["std_dev"] != 512 || out.Params["std_dev"] != 256 {
		t.Fatalf("std_dev should be cov*mean: in=%v out=%v", in.Params["std_dev"], out.Params["std_dev"])
	}
	if in.Params["min"] != 1 || out.Params["min"] != 1 {
		t.Fatalf("min should be 1: in=%v out=%v", in.Params["min"], out.Params["min"])
	}
	if sum := in.Params["max"] + out.Params["max"]; sum != 4096 {
		t.Fatalf("max_in + max_out = %v, want 4096 (sum-split)", sum)
	}
	if in.Params["max"] <= out.Params["max"] {
		t.Fatalf("max_in (%v) should exceed max_out (%v) for mean ratio 2:1", in.Params["max"], out.Params["max"])
	}
}

// Gaussian samples stay within [1, max] so the DES never sees an over-cap draw.
func TestTokenDist_GaussianSamplesWithinBounds(t *testing.T) {
	spec := tokenDist(&tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 1}, 1024, 512, 4096)
	s, err := workload.NewLengthSampler(spec)
	if err != nil {
		t.Fatalf("NewLengthSampler: %v", err)
	}
	maxCap := int(spec.Params["max"])
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 10000; i++ {
		if v := s.Sample(rng); v < 1 || v > maxCap {
			t.Fatalf("sample %d outside [1,%d]", v, maxCap)
		}
	}
}

// Gaussian input cap + output cap summing to MaxModelLen guarantees no per-request
// sequence exceeds it.
func TestTokenDist_GaussianInputPlusOutputNeverExceedsMaxModelLen(t *testing.T) {
	const maxModelLen = 4096
	cfg := &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 1}
	inS, _ := workload.NewLengthSampler(tokenDist(cfg, 1024, 512, maxModelLen))
	outS, _ := workload.NewLengthSampler(tokenDist(cfg, 512, 1024, maxModelLen))
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 10000; i++ {
		if seq := inS.Sample(rng) + outS.Sample(rng); seq > maxModelLen {
			t.Fatalf("input+output = %d exceeds MaxModelLen %d", seq, maxModelLen)
		}
	}
}

// Lognormal mu/sigma are derived so the distribution's mean matches the request
// mean; the sum-split max is present and samples stay within [1, max].
func TestTokenDist_Lognormal(t *testing.T) {
	const mean, otherMean, maxModelLen = 1024.0, 1024.0, 1_000_000.0
	spec := tokenDist(&tokenDistConfig{Type: "lognormal", Cov: 0.5, Min: 1}, mean, otherMean, maxModelLen)
	if spec.Type != "lognormal" {
		t.Fatalf("type = %q, want lognormal", spec.Type)
	}
	// sigma = sqrt(ln(1+cov^2)); mu = ln(mean) - sigma^2/2.
	wantSigma := math.Sqrt(math.Log(1 + 0.25))
	if math.Abs(spec.Params["sigma"]-wantSigma) > 1e-9 {
		t.Fatalf("sigma = %v, want %v", spec.Params["sigma"], wantSigma)
	}
	wantMu := math.Log(mean) - wantSigma*wantSigma/2
	if math.Abs(spec.Params["mu"]-wantMu) > 1e-9 {
		t.Fatalf("mu = %v, want %v", spec.Params["mu"], wantMu)
	}
	if spec.Params["max"] != maxModelLen*mean/(mean+otherMean) {
		t.Fatalf("max = %v, want sum-split share", spec.Params["max"])
	}
	// Sampled mean should reproduce the target mean (max is huge → clamping rare).
	s, err := workload.NewLengthSampler(spec)
	if err != nil {
		t.Fatalf("NewLengthSampler: %v", err)
	}
	rng := rand.New(rand.NewSource(99))
	const n = 100000
	sum := 0
	for i := 0; i < n; i++ {
		sum += s.Sample(rng)
	}
	if got := float64(sum) / n; math.Abs(got-mean) > 100 {
		t.Fatalf("sampled mean = %v, want within 100 of %v", got, mean)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./blis-evaluator/ -run TestTokenDist -v`
Expected: compile error — `tokenDist` is called with 4 args but the current definition takes 3.

- [ ] **Step 3: Rewrite `tokenDist` and add `sumSplitMax`**

In `blis-evaluator/handler.go`, add `"math"` to the import block (alongside `net/http`, `os`).

Replace the entire existing `tokenDist` function (the one with the `func tokenDist(mean, otherMean, maxModelLen float64)` signature and its doc comment) with:

```go
// tokenDist builds a token-length distribution for one dimension (input or
// output) from its mean. The request supplies the mean (ProblemData carries
// only means); cfg supplies the distribution type, coefficient of variation,
// and clamp floor; and a positive maxModelLen supplies the clamp ceiling via a
// sum-split against the other dimension's mean — so input + output budgets
// always sum to <= maxModelLen (blis drops requests whose input+output exceeds
// it). A nil cfg means the constant (fixed-length) default.
//
// Config validation (validateTokenDist) guarantees exponential is never paired
// with maxModelLen > 0, so its unclampable tail can never breach the bound.
func tokenDist(cfg *tokenDistConfig, mean, otherMean, maxModelLen float64) workload.DistSpec {
	if cfg == nil || cfg.Type == "constant" {
		return workload.DistSpec{
			Type:   "constant",
			Params: map[string]float64{"value": math.Round(mean)},
		}
	}

	switch cfg.Type {
	case "exponential":
		return workload.DistSpec{
			Type:   "exponential",
			Params: map[string]float64{"mean": mean},
		}
	case "gaussian":
		return workload.DistSpec{
			Type: "gaussian",
			Params: map[string]float64{
				"mean":    mean,
				"std_dev": cfg.Cov * mean,
				"min":     cfg.Min,
				"max":     sumSplitMax(mean, otherMean, maxModelLen),
			},
		}
	case "lognormal":
		sigma := math.Sqrt(math.Log(1 + cfg.Cov*cfg.Cov))
		mu := math.Log(mean) - sigma*sigma/2
		return workload.DistSpec{
			Type: "lognormal",
			Params: map[string]float64{
				"mu":    mu,
				"sigma": sigma,
				"min":   cfg.Min,
				"max":   sumSplitMax(mean, otherMean, maxModelLen),
			},
		}
	default:
		// Unreachable: validateTokenDist rejects unknown types at config load.
		return workload.DistSpec{
			Type:   "constant",
			Params: map[string]float64{"value": math.Round(mean)},
		}
	}
}

// sumSplitMax returns this dimension's share of maxModelLen, split by the mean
// ratio against the other dimension, so the input and output clamp ceilings sum
// to maxModelLen and per-request input + output never exceeds it. Floored at 1.
func sumSplitMax(mean, otherMean, maxModelLen float64) float64 {
	share := maxModelLen
	if mean+otherMean > 0 {
		share = maxModelLen * mean / (mean + otherMean)
	}
	if share < 1 {
		share = 1
	}
	return share
}
```

- [ ] **Step 4: Update the two call sites**

In `blis-evaluator/handler.go`, in the `WorkloadSpec` literal, change:

```go
					InputDist:    tokenDist(inMean, outMean, maxModelLen),
					OutputDist:   tokenDist(outMean, inMean, maxModelLen),
```

to:

```go
					InputDist:    tokenDist(entry.TokenDist, inMean, outMean, maxModelLen),
					OutputDist:   tokenDist(entry.TokenDist, outMean, inMean, maxModelLen),
```

Also update the comment block immediately above `maxModelLen := float64(entry.MaxModelLen)` so it no longer claims a hardcoded gaussian/exponential. Replace that comment with:

```go
		// Build a single-client workload whose token-length distributions are
		// synthesized by tokenDist from the per-entry tokenDist config (type,
		// cov, min) and the MaxModelLen sum-split. A nil config defaults to a
		// constant (fixed-length) distribution.
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./blis-evaluator/ -run TestTokenDist -v`
Expected: PASS (all `TestTokenDist*` subtests).

- [ ] **Step 6: Verify the whole repo builds and all tests pass**

Run: `go build ./... && go test ./...`
Expected: build succeeds; all packages pass.

- [ ] **Step 7: Commit**

```bash
git add blis-evaluator/handler.go blis-evaluator/tokendist_test.go
git commit -m "feat(blis): synthesize token distribution from tokenDist config"
```

---

### Task 3: Migrate existing config entries and update docs

**Files:**
- Modify: `blis-evaluator/blis-config.json` (add a `tokenDist` block to every entry, preserving current behavior)
- Modify: `docs/blis-evaluator-config.md` (document the new field)

**Interfaces:**
- Consumes: validation from Task 1 and synthesis from Task 2 — config must load cleanly under the new rules.

- [ ] **Step 1: Add `tokenDist` blocks to `blis-config.json`**

For the single entry with `"maxModelLen": 4096` (`Qwen/Qwen2.5-14B-Instruct` / H100), add after its `"seed"` field (mind the trailing comma on `seed`):

```json
      "seed": 42,
      "tokenDist": { "type": "gaussian", "cov": 0.5, "min": 1 }
```

For every entry with `"maxModelLen": 0` (the other 10 entries: granite-3.1-8b ×2, granite-34b-code ×2, Llama-2-13b ×2, Llama-2-70b ×2, Mixtral-8x7B ×2), add after each `"seed"` field:

```json
      "seed": 42,
      "tokenDist": { "type": "exponential" }
```

- [ ] **Step 2: Verify the config loads and the JSON is valid**

Run: `cd blis-evaluator && BLIS_CONFIG_FILE=blis-config.json go run . & sleep 2; curl -s localhost:8081/healthz 2>/dev/null; kill %1 2>/dev/null; cd ..`

Expected: the server starts without a config-load fatal error (no `invalid config entry` log). If the evaluator has no `/healthz`, the absence of a startup panic is the signal. Alternatively just confirm JSON validity:

Run: `python3 -c "import json; json.load(open('blis-evaluator/blis-config.json')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Document the field in `docs/blis-evaluator-config.md`**

In the "Optional (defaulted)" table (around line 53), add a row after the `seed` row:

```markdown
| `tokenDist` | `constant` (fixed length) | Token-length distribution. See [Token-length distribution](#token-length-distribution). |
```

Then add a new `###` section immediately after the "Optional (defaulted)" table (before "### Where `totalKVBlocks` comes from"):

```markdown
### Token-length distribution

`tokenDist` controls how per-request input/output token counts are sampled. The
request supplies only the **mean** (`AvgInputTokens`/`AvgOutputTokens`); `cov`
and `min` come from this block; the clamp ceiling is derived from `maxModelLen`.

```jsonc
"tokenDist": {
  "type": "gaussian",   // "constant" | "exponential" | "gaussian" | "lognormal"
  "cov": 0.5,            // coefficient of variation (std_dev/mean); gaussian & lognormal
  "min": 1               // clamp floor (default 1)
}
```

| Type | Spread | Bounded by `maxModelLen`? |
|------|--------|---------------------------|
| `constant` (default) | none — fixed `value = round(mean)` | n/a (deterministic) |
| `exponential` | CoV fixed at 1 | no — **rejected at config load when `maxModelLen > 0`** |
| `gaussian` | `std_dev = cov * mean` | yes (clamped) |
| `lognormal` | `sigma = sqrt(ln(1+cov²))`, mean preserved | yes (clamped) |

When `maxModelLen > 0`, the input and output caps **sum-split** `maxModelLen` by
the mean ratio, so per-request `input + output <= maxModelLen` by construction.
`exponential` is unclampable (its sampler ignores `min`/`max`) and is therefore
rejected at config load when `maxModelLen > 0`; use `gaussian` or `lognormal`
for bounded models. Omitting `tokenDist` yields a `constant` (fixed-length)
workload.
```

- [ ] **Step 4: Verify docs render and links resolve**

Run: `grep -n "Token-length distribution" docs/blis-evaluator-config.md`
Expected: two matches — the table-row link and the section heading.

- [ ] **Step 5: Commit**

```bash
git add blis-evaluator/blis-config.json docs/blis-evaluator-config.md
git commit -m "feat(blis): migrate config entries to explicit tokenDist; document field"
```

---

### Task 4: Rescope PR #38 to close #39

**Files:** none (GitHub metadata only).

- [ ] **Step 1: Push the branch**

```bash
git push
```

- [ ] **Step 2: Rescope the PR title and body**

```bash
gh pr edit 38 --title "blis: configurable token-length distribution (bounded by MaxModelLen)"
```

Then update the body to describe the configurable feature and close the issue:

```bash
gh pr edit 38 --body "$(cat <<'EOF'
Generalizes the token-length distribution in blis-evaluator from a hardcoded
clamped-gaussian/exponential into a per-entry `tokenDist` config block
(`type` + `cov` + `min`).

- Types: `constant` (default), `exponential`, `gaussian`, `lognormal`.
- The request supplies the mean; `cov`/`min` come from config; the clamp `max`
  is the `MaxModelLen` sum-split (input+output <= MaxModelLen preserved).
- `exponential` is rejected at config load when `MaxModelLen > 0` (unclampable).
- Existing config entries migrated to explicit `tokenDist` blocks to preserve
  current behavior.

Design: docs/superpowers/specs/2026-06-28-blis-configurable-token-dist-design.md

Closes #39
EOF
)"
```

- [ ] **Step 3: Verify**

Run: `gh pr view 38 --json title,body | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['title']); print('Closes #39' in d['body'])"`
Expected: the new title, then `True`.

---

## Self-Review

**Spec coverage:**
- Config schema (`tokenDistConfig`, `TokenDist` field) → Task 1. ✅
- `cov` unified spread knob; gaussian/lognormal derivation → Task 2. ✅
- Type→DistSpec translation table (constant/exponential/gaussian/lognormal) → Task 2. ✅
- Sum-split `max` preserved for clampable types → Task 2 (`sumSplitMax`). ✅
- Default = constant; nil block → constant → Task 2 + test. ✅
- Validation (type set, exponential+maxModelLen>0 reject, cov>0, min>=0, stray-param warning) → Task 1. ✅
- `min` default = 1 when block present → Task 1 (`applyDefaults`). ✅
- Migrate existing entries (Qwen gaussian; 10 others exponential) → Task 3. ✅
- Docs update → Task 3. ✅
- Tests (per-type synthesis, bounding, validation table) → Tasks 1 & 2. ✅
- Rescope #38 to close #39 → Task 4. ✅

**Placeholder scan:** No TBD/TODO; every code/step shows concrete content. ✅

**Type consistency:** `tokenDistConfig{Type, Cov, Min}` and `tokenDist(cfg *tokenDistConfig, mean, otherMean, maxModelLen float64)` and `sumSplitMax(mean, otherMean, maxModelLen float64)` used identically across Tasks 1–2 and tests. ✅

**Edge note (acknowledged in spec, not a gap):** `lognormal`/`constant` with `mean <= 0` would yield a degenerate spec; the library samplers degrade to `min`/`1` rather than crash, and means are positive in practice. No extra task warranted.
