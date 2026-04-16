package evaluator

import "testing"

func TestIsSaturated(t *testing.T) {
	tests := []struct {
		name       string
		saturation string
		want       bool
	}{
		{"empty string is not saturated", SaturationNone, false},
		{"bandwidth is saturated", SaturationBandwidth, true},
		{"kv_capacity is saturated", SaturationKV, true},
		{"overloaded is saturated", SaturationOverload, true},
		{"arbitrary non-empty string is saturated", "some_future_value", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ad := AnalysisData{Saturation: tc.saturation}
			if got := ad.IsSaturated(); got != tc.want {
				t.Errorf("IsSaturated() = %v, want %v (saturation=%q)", got, tc.want, tc.saturation)
			}
		})
	}
}
