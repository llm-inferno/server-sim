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
	if sum := in.Params["max"] + out.Params["max"]; math.Abs(sum-4096) > 1e-9 {
		t.Fatalf("max_in + max_out = %v, want 4096 (sum-split)", sum)
	}
	if in.Params["max"] <= out.Params["max"] {
		t.Fatalf("max_in (%v) should exceed max_out (%v) for mean ratio 2:1", in.Params["max"], out.Params["max"])
	}
}

// When a skewed mean ratio shrinks this dimension's sum-split share below the
// configured min, the emitted min is capped at max so the sampler sees a
// coherent [min, max] band (never min > max).
func TestTokenDist_MinCappedAtSumSplitMax(t *testing.T) {
	// mean 10 vs otherMean 4086 → share ≈ 10, well below the configured min 100.
	cfg := &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 100}
	d := tokenDist(cfg, 10, 4086, 4096)
	if d.Params["min"] > d.Params["max"] {
		t.Fatalf("min (%v) must not exceed max (%v)", d.Params["min"], d.Params["max"])
	}
	if d.Params["min"] != d.Params["max"] {
		t.Fatalf("min should be capped at max (%v), got %v", d.Params["max"], d.Params["min"])
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
