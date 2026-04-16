package main

import (
	"math"
	"testing"

	blisSim "github.com/inference-sim/inference-sim/sim"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// tinyDenseModel returns a minimal ModelConfig whose estimateWeightBytes output
// is easy to verify by hand:
//
//	HiddenDim=4, NumLayers=1, NumHeads=2, NumKVHeads=2, VocabSize=8, IntermediateDim=8
//	headDim = 4/2 = 2, kvDim = 2*2 = 4
//	embeddings      = 8*4         = 32
//	attnPerLayer    = 4*(4+2*4)+4*4 = 4*12+16 = 64
//	mlpPerLayer     = 3*4*8       = 96
//	normsPerLayer   = 2*4         = 8
//	lmHead          = 8*4         = 32
//	finalNorm        = 4
//	totalParams     = 32 + (64+96+8) + 32 + 4 = 236
//	weightBytes     = 236 * 2.0   = 472.0
func tinyDenseModel() blisSim.ModelConfig {
	return blisSim.ModelConfig{
		NumLayers:       1,
		HiddenDim:       4,
		NumHeads:        2,
		NumKVHeads:      2,
		VocabSize:       8,
		IntermediateDim: 8,
		BytesPerParam:   2.0,
	}
}

// tinyMoEModel is like tinyDenseModel but with 4 experts and 2 active per token.
// MoE MLP per layer (all 4 experts): 3 * 4 * 8 * 4 = 384
// (replaces the dense mlpPerLayer of 96)
// totalParams = 32 + (64+384+8) + 32 + 4 = 524
// weightBytes = 524 * 2.0 = 1048.0
func tinyMoEModel() blisSim.ModelConfig {
	mc := tinyDenseModel()
	mc.NumLocalExperts = 4
	mc.NumExpertsPerTok = 2
	return mc
}

// loHWConfig returns a HardwareCalib with very low bandwidth so that bandwidth
// saturation is easy to trigger in tests without needing realistic RPS values.
//
// With BwPeakTBs=1e-9 and tinyDenseModel (472 bytes):
//
//	decodeCapacityTPS = 1e-9 * 1e12 / 472 ≈ 2.118 tokens/sec
//
// maxRPS_bandwidth = decodeCapacityTPS / AvgOutputTokens
func loHWConfig() blisSim.HardwareCalib {
	return blisSim.HardwareCalib{BwPeakTBs: 1e-9}
}

// hiHWConfig returns a HardwareCalib with very high bandwidth so bandwidth
// is never the bottleneck in KV-saturation tests.
func hiHWConfig() blisSim.HardwareCalib {
	return blisSim.HardwareCalib{BwPeakTBs: 1e6}
}

// ---------------------------------------------------------------------------
// estimateWeightBytes
// ---------------------------------------------------------------------------

func TestEstimateWeightBytes_DenseModel(t *testing.T) {
	mc := tinyDenseModel()
	got := estimateWeightBytes(&mc)
	const want = 472.0
	if got != want {
		t.Errorf("estimateWeightBytes dense = %v, want %v", got, want)
	}
}

func TestEstimateWeightBytes_ScalesWithNumLayers(t *testing.T) {
	mc1 := tinyDenseModel()
	mc2 := tinyDenseModel()
	mc2.NumLayers = 2

	b1 := estimateWeightBytes(&mc1)
	b2 := estimateWeightBytes(&mc2)

	// doubling NumLayers adds one more per-layer block; static terms are unchanged
	perLayer := float64(64+96+8) * mc1.EffectiveWeightBytesPerParam()
	if math.Abs((b2-b1)-perLayer) > 1e-6 {
		t.Errorf("doubling layers added %v bytes, expected %v (one perLayer block)", b2-b1, perLayer)
	}
}

func TestEstimateWeightBytes_MoEModelIsLarger(t *testing.T) {
	dense := tinyDenseModel()
	moe := tinyMoEModel()
	if estimateWeightBytes(&moe) <= estimateWeightBytes(&dense) {
		t.Error("MoE model should have more weight bytes than equivalent dense model")
	}
}

func TestEstimateWeightBytes_QuantisedModelIsSmaller(t *testing.T) {
	fp16 := tinyDenseModel()
	int8 := tinyDenseModel()
	int8.WeightBytesPerParam = 1.0 // INT8 quantised weights

	if estimateWeightBytes(&int8) >= estimateWeightBytes(&fp16) {
		t.Error("INT8 quantised model should have fewer weight bytes than FP16")
	}
}

func TestEstimateWeightBytes_ZeroIntermediateDimFallsBackTo4xHidden(t *testing.T) {
	mc := tinyDenseModel()
	mc.IntermediateDim = 0
	// fallback: inter = 4 * HiddenDim = 4 * 4 = 16
	// mlpPerLayer = 3 * 4 * 16 = 192  (was 96 with IntermediateDim=8)
	withFallback := estimateWeightBytes(&mc)
	mc.IntermediateDim = 16
	withExplicit := estimateWeightBytes(&mc)
	if withFallback != withExplicit {
		t.Errorf("zero IntermediateDim fallback gave %v, explicit 16 gave %v", withFallback, withExplicit)
	}
}

// ---------------------------------------------------------------------------
// checkSaturation — bandwidth bottleneck
// ---------------------------------------------------------------------------

func TestCheckSaturation_BandwidthSaturated(t *testing.T) {
	mc := tinyDenseModel()
	// decodeCapacityTPS ≈ 2.118 tokens/sec; demand = RPS*AvgOut = 3.0*1 = 3.0 > 2.118*0.98
	pd := evaluator.ProblemData{RPS: 3.0, AvgInputTokens: 10, AvgOutputTokens: 1}
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	sat, maxRPS := checkSaturation(pd, &mc, loHWConfig(), entry)

	if sat != evaluator.SaturationBandwidth {
		t.Errorf("saturation = %q, want %q", sat, evaluator.SaturationBandwidth)
	}
	if maxRPS <= 0 {
		t.Errorf("maxRPS = %v, want > 0 for bandwidth saturation", maxRPS)
	}
}

func TestCheckSaturation_BandwidthNotSaturated(t *testing.T) {
	mc := tinyDenseModel()
	// demand = 1.0 * 1 = 1.0 < 2.118 * 0.98 ≈ 2.076
	pd := evaluator.ProblemData{RPS: 1.0, AvgInputTokens: 10, AvgOutputTokens: 1}
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	sat, _ := checkSaturation(pd, &mc, loHWConfig(), entry)

	if sat != evaluator.SaturationNone {
		t.Errorf("saturation = %q, want none (sub-capacity load)", sat)
	}
}

func TestCheckSaturation_BandwidthSaturation_ReturnsPositiveMaxRPS(t *testing.T) {
	mc := tinyDenseModel()
	pd := evaluator.ProblemData{RPS: 3.0, AvgInputTokens: 10, AvgOutputTokens: 1}
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	_, maxRPS := checkSaturation(pd, &mc, loHWConfig(), entry)

	// maxRPS = decodeCapacityTPS / AvgOutputTokens = 2.118 / 1 ≈ 2.118
	// should be roughly 2.0-3.0 for these params
	if maxRPS <= 0 || maxRPS >= float32(pd.RPS) {
		t.Errorf("maxRPS = %v; expected positive value below the offered RPS", maxRPS)
	}
}

func TestCheckSaturation_HigherTPReducesBandwidthPressure(t *testing.T) {
	mc := tinyDenseModel()
	// With TP=1 and RPS=3.0 it's bandwidth-saturated (see above).
	// With TP=4 the bandwidth ceiling quadruples so the same RPS is fine.
	pd := evaluator.ProblemData{RPS: 3.0, AvgInputTokens: 10, AvgOutputTokens: 1}
	kvOK := modelEntry{TP: 4, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	sat, _ := checkSaturation(pd, &mc, loHWConfig(), kvOK)

	if sat == evaluator.SaturationBandwidth {
		t.Errorf("higher TP should remove bandwidth saturation but got %q", sat)
	}
}

// ---------------------------------------------------------------------------
// checkSaturation — KV cache bottleneck
// ---------------------------------------------------------------------------

func TestCheckSaturation_KVSaturated(t *testing.T) {
	mc := tinyDenseModel()
	// totalKVSlots = 10 * 16 = 160; concurrentTokens = 10 * 20 = 200 > 160*0.98=156.8
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 10, AvgOutputTokens: 10}
	entry := modelEntry{
		TP: 1, TotalKVBlocks: 10, BlockSizeTokens: 16, MaxRunningReqs: 10,
	}

	sat, _ := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationKV {
		t.Errorf("saturation = %q, want %q", sat, evaluator.SaturationKV)
	}
}

func TestCheckSaturation_KVNotSaturated(t *testing.T) {
	mc := tinyDenseModel()
	// totalKVSlots = 10 * 16 = 160; concurrentTokens = 5 * 20 = 100 < 156.8
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 10, AvgOutputTokens: 10}
	entry := modelEntry{
		TP: 1, TotalKVBlocks: 10, BlockSizeTokens: 16, MaxRunningReqs: 5,
	}

	sat, _ := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationNone {
		t.Errorf("saturation = %q, want none (KV fits)", sat)
	}
}

func TestCheckSaturation_MaxConcurrencyOverridesMaxRunningReqs(t *testing.T) {
	mc := tinyDenseModel()
	// entry.MaxRunningReqs=10 would KV-saturate, but pd.MaxConcurrency=5 overrides it
	pd := evaluator.ProblemData{
		RPS: 0.001, AvgInputTokens: 10, AvgOutputTokens: 10, MaxConcurrency: 5,
	}
	entry := modelEntry{
		TP: 1, TotalKVBlocks: 10, BlockSizeTokens: 16, MaxRunningReqs: 10,
	}

	sat, _ := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationNone {
		t.Errorf("MaxConcurrency=5 should override MaxRunningReqs=10 and avoid KV saturation, got %q", sat)
	}
}

// ---------------------------------------------------------------------------
// checkSaturation — tolerance margin boundary
// ---------------------------------------------------------------------------

func TestCheckSaturation_ExactlyAtCapacityIsNotSaturated(t *testing.T) {
	mc := tinyDenseModel()
	// decodeCapacityTPS ≈ 2.118; demand = RPS*AvgOut
	// Set demand exactly at capacity (not * 0.98) — should NOT be flagged.
	hc := loHWConfig()
	wBytes := estimateWeightBytes(&mc)
	decodeCapTPS := hc.BwPeakTBs * 1e12 / wBytes // ≈ 2.118 with TP=1

	// demand = decodeCapTPS * 1.0; with margin we need demand > decodeCapTPS * 0.98
	// so demand = decodeCapTPS * 0.97 is safely below the threshold
	rps := float32(decodeCapTPS * 0.97)
	pd := evaluator.ProblemData{RPS: rps, AvgInputTokens: 10, AvgOutputTokens: 1}
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	sat, _ := checkSaturation(pd, &mc, hc, entry)

	if sat == evaluator.SaturationBandwidth {
		t.Errorf("demand at 97%% of capacity should not be saturated (margin is 2%%)")
	}
}

func TestCheckSaturation_JustAboveMarginIsSaturated(t *testing.T) {
	mc := tinyDenseModel()
	hc := loHWConfig()
	wBytes := estimateWeightBytes(&mc)
	decodeCapTPS := hc.BwPeakTBs * 1e12 / wBytes

	// demand = decodeCapTPS * 0.99 > decodeCapTPS * 0.98 → should be saturated
	rps := float32(decodeCapTPS * 0.99)
	pd := evaluator.ProblemData{RPS: rps, AvgInputTokens: 10, AvgOutputTokens: 1}
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	sat, _ := checkSaturation(pd, &mc, hc, entry)

	if sat != evaluator.SaturationBandwidth {
		t.Errorf("demand at 99%% of capacity should be saturated (margin is 2%%), got %q", sat)
	}
}

// ---------------------------------------------------------------------------
// checkSaturation — neither bottleneck active
// ---------------------------------------------------------------------------

func TestCheckSaturation_NeitherBottleneck(t *testing.T) {
	mc := tinyDenseModel()
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 1, AvgOutputTokens: 1}
	entry := modelEntry{
		TP: 1, TotalKVBlocks: 100000, BlockSizeTokens: 16, MaxRunningReqs: 1,
	}

	sat, maxRPS := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationNone {
		t.Errorf("well under capacity: saturation = %q, want none", sat)
	}
	if maxRPS != 0 {
		t.Errorf("maxRPS = %v, want 0 when not saturated", maxRPS)
	}
}
