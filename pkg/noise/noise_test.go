package noise

import (
	"math/rand"
	"testing"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func TestAddNoise_PreservesSaturationField(t *testing.T) {
	cfg := Config{StdFraction: 0.5} // large noise so numeric changes are obvious

	tests := []struct {
		name       string
		saturation string
	}{
		{"empty saturation preserved", evaluator.SaturationNone},
		{"bandwidth saturation preserved", evaluator.SaturationBandwidth},
		{"kv_capacity saturation preserved", evaluator.SaturationKV},
		{"overloaded saturation preserved", evaluator.SaturationOverload},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ad := evaluator.AnalysisData{
				Throughput:  10,
				AvgRespTime: 100,
				Saturation:  tc.saturation,
			}
			got := AddNoise(ad, cfg)
			if got.Saturation != tc.saturation {
				t.Errorf("Saturation = %q, want %q", got.Saturation, tc.saturation)
			}
		})
	}
}

func TestAddNoise_PerturbsNonZeroFields(t *testing.T) {
	// Fix the seed so the test is deterministic. With StdFraction=0.5 and
	// NormFloat64 != 0 (which is almost certain over many runs), the noisy
	// value should differ from the original.
	rand.Seed(42)
	cfg := Config{StdFraction: 0.5}

	ad := evaluator.AnalysisData{
		Throughput:  10,
		AvgRespTime: 100,
		AvgWaitTime: 50,
		AvgTTFT:     30,
		AvgITL:      5,
		MaxRPS:      15,
	}

	// Run many samples to confirm at least one field differs (noise is nonzero).
	anyChanged := false
	for i := 0; i < 20; i++ {
		got := AddNoise(ad, cfg)
		if got.Throughput != ad.Throughput ||
			got.AvgRespTime != ad.AvgRespTime ||
			got.MaxRPS != ad.MaxRPS {
			anyChanged = true
			break
		}
	}
	if !anyChanged {
		t.Error("AddNoise did not change any field after 20 samples with StdFraction=0.5")
	}
}

func TestAddNoise_ZeroFieldsUnchanged(t *testing.T) {
	cfg := Config{StdFraction: 0.5}
	ad := evaluator.AnalysisData{} // all zeros

	for i := 0; i < 10; i++ {
		got := AddNoise(ad, cfg)
		if got.Throughput != 0 || got.AvgRespTime != 0 || got.MaxRPS != 0 {
			t.Errorf("zero-valued fields should remain 0 after noise, got %+v", got)
		}
	}
}

func TestAddNoise_NeverProducesNegative(t *testing.T) {
	cfg := Config{StdFraction: 5.0} // extreme noise
	ad := evaluator.AnalysisData{
		Throughput:  1,
		AvgRespTime: 1,
		AvgWaitTime: 1,
		AvgTTFT:     1,
		AvgITL:      1,
		MaxRPS:      1,
	}

	for i := 0; i < 100; i++ {
		got := AddNoise(ad, cfg)
		if got.Throughput < 0 || got.AvgRespTime < 0 || got.AvgWaitTime < 0 ||
			got.AvgTTFT < 0 || got.AvgITL < 0 || got.MaxRPS < 0 {
			t.Errorf("AddNoise produced a negative field: %+v", got)
		}
	}
}
