package main

import (
	"math"
	"math/rand"
	"testing"
)

func empiricalStats(t *testing.T, s tokenSampler, n int, seed int64) (mean float64, lo, hi int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	lo = math.MaxInt
	sum := 0
	for i := 0; i < n; i++ {
		v := s.Sample(rng)
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
		sum += v
	}
	mean = float64(sum) / float64(n)
	return
}

func TestFixedSampler(t *testing.T) {
	s, err := newSampler("fixed", 7)
	if err != nil {
		t.Fatal(err)
	}
	mean, lo, hi := empiricalStats(t, s, 1000, 1)
	if mean != 7 || lo != 7 || hi != 7 {
		t.Errorf("fixed(7): mean=%v lo=%d hi=%d, want 7/7/7", mean, lo, hi)
	}
}

func TestFixedSampler_DefaultEmptyKind(t *testing.T) {
	s, err := newSampler("", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(fixedSampler); !ok {
		t.Errorf("empty kind should default to fixedSampler, got %T", s)
	}
	if s.Sample(rand.New(rand.NewSource(1))) != 5 {
		t.Errorf("fixed(5) sample != 5")
	}
}

func TestGeometricSampler_Mean(t *testing.T) {
	// Mean is the testable property at production-shape parameters: with
	// hi = 10*avg the truncation tail is ~e^-10, so empirical mean is
	// indistinguishable from the untruncated value within tolerance.
	const avg = 16
	s, err := newSampler("geometric", avg)
	if err != nil {
		t.Fatal(err)
	}
	mean, lo, _ := empiricalStats(t, s, 20_000, 42)
	if math.Abs(mean-float64(avg)) > 1.0 {
		t.Errorf("geometric mean = %v, want ~%d (within 1.0)", mean, avg)
	}
	if lo < 1 {
		t.Errorf("geometric lo = %d, want ≥ 1", lo)
	}
}

// TestGeometricSampler_RespectsCap exercises the upper-bound clamp directly.
// At p=0.5, hi=3, P(unclamped X > 3) = 0.5^3 = 12.5%, so the cap is hit
// frequently enough to verify both that clamped samples never exceed hi and
// that the clamp is reached often enough to be meaningfully tested.
func TestGeometricSampler_RespectsCap(t *testing.T) {
	s := geometricSampler{p: 0.5, hi: 3}
	rng := rand.New(rand.NewSource(1))
	const N = 10_000
	capHits := 0
	for i := 0; i < N; i++ {
		v := s.Sample(rng)
		if v < 1 || v > 3 {
			t.Fatalf("sample = %d, want in [1,3]", v)
		}
		if v == 3 {
			capHits++
		}
	}
	// True P(X >= 3) at p=0.5 is 0.25 → expect ~2500 hits at v=3.
	if capHits < 1000 {
		t.Errorf("cap hits = %d in %d draws; expected ~2500. Truncation may not be exercised.", capHits, N)
	}
}

func TestUniformSampler_MeanAndBounds(t *testing.T) {
	const avg = 8
	s, err := newSampler("uniform", avg)
	if err != nil {
		t.Fatal(err)
	}
	mean, lo, hi := empiricalStats(t, s, 20_000, 7)
	if math.Abs(mean-float64(avg)) > 0.3 {
		t.Errorf("uniform mean = %v, want ~%d", mean, avg)
	}
	if lo < 1 {
		t.Errorf("uniform lo = %d, want ≥ 1", lo)
	}
	if hi > 2*avg-1 {
		t.Errorf("uniform hi = %d, want ≤ %d", hi, 2*avg-1)
	}
}

func TestUniformBoundedSampler_MeanAndBounds(t *testing.T) {
	const avg = 10
	s, err := newSampler("uniform-bounded", avg)
	if err != nil {
		t.Fatal(err)
	}
	mean, lo, hi := empiricalStats(t, s, 20_000, 99)
	if math.Abs(mean-float64(avg)) > 0.3 {
		t.Errorf("uniform-bounded mean = %v, want ~%d", mean, avg)
	}
	wantLo := avg / 2
	wantHi := (3*avg + 1) / 2
	if lo < wantLo {
		t.Errorf("uniform-bounded lo = %d, want ≥ %d", lo, wantLo)
	}
	if hi > wantHi {
		t.Errorf("uniform-bounded hi = %d, want ≤ %d", hi, wantHi)
	}
}

func TestUniformBoundedSampler_AvgTwoLowerBound(t *testing.T) {
	// avg=2: lo = max(1, 1) = 1, hi = ceil(3) = 3 → support {1,2,3}, mean = 2.
	s, err := newSampler("uniform-bounded", 2)
	if err != nil {
		t.Fatal(err)
	}
	mean, lo, hi := empiricalStats(t, s, 20_000, 5)
	if lo < 1 || hi > 3 {
		t.Errorf("avg=2: support [%d,%d], want [1,3]", lo, hi)
	}
	if math.Abs(mean-2.0) > 0.1 {
		t.Errorf("avg=2 mean = %v, want ~2", mean)
	}
}

func TestAllSamplers_AvgOneCollapsesToFixed(t *testing.T) {
	for _, kind := range []string{"fixed", "geometric", "uniform", "uniform-bounded"} {
		s, err := newSampler(kind, 1)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		mean, lo, hi := empiricalStats(t, s, 1000, 13)
		if mean != 1 || lo != 1 || hi != 1 {
			t.Errorf("%s avg=1: mean=%v lo=%d hi=%d, want 1/1/1", kind, mean, lo, hi)
		}
	}
}

func TestNewSampler_UnknownKindFails(t *testing.T) {
	if _, err := newSampler("lognormal", 8); err == nil {
		t.Errorf("expected error for unknown distribution kind")
	}
}

func TestNewSampler_KindIsCaseInsensitiveAndTrimmed(t *testing.T) {
	if _, err := newSampler("  Geometric ", 8); err != nil {
		t.Errorf("expected normalization to accept '  Geometric ': %v", err)
	}
}
