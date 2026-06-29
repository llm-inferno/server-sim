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
// With BwPeakTBs=1e-9 (bwBytesPerSec = 1e3), tinyDenseModel (weightBytes=472,
// kvBytesPerToken=16) and a batch of 1 at in=10/out=1 (avgContext=10.5):
//
//	t_step    = (472 + 1*10.5*16) / 1e3 = 0.64 s
//	decodeTPS = 1 / 0.64 ≈ 1.5625 tokens/sec
//
// maxRPS_bandwidth = decodeTPS / AvgOutputTokens
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

	sat, ad := checkSaturation(pd, &mc, loHWConfig(), entry)

	if sat != evaluator.SaturationBandwidth {
		t.Errorf("saturation = %q, want %q", sat, evaluator.SaturationBandwidth)
	}
	if ad.MaxRPS <= 0 {
		t.Errorf("maxRPS = %v, want > 0 for bandwidth saturation", ad.MaxRPS)
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

	_, ad := checkSaturation(pd, &mc, loHWConfig(), entry)

	// maxRPS = decodeCapacityTPS / AvgOutputTokens = 2.118 / 1 ≈ 2.118
	// should be roughly 2.0-3.0 for these params
	if ad.MaxRPS <= 0 || ad.MaxRPS >= float32(pd.RPS) {
		t.Errorf("maxRPS = %v; expected positive value below the offered RPS", ad.MaxRPS)
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
	// Degenerate KV saturation: a single average request's context does not fit.
	// totalKVSlots = 1*16 = 16; avgContext = 20 + 10/2 = 25 > 16 → B_kv < 1.
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 20, AvgOutputTokens: 10}
	entry := modelEntry{
		TP: 1, TotalKVBlocks: 1, BlockSizeTokens: 16, MaxRunningReqs: 10,
	}

	sat, _ := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationKV {
		t.Errorf("saturation = %q, want %q", sat, evaluator.SaturationKV)
	}
}

func TestCheckSaturation_HighMaxConcurrencyDoesNotFalselySaturate(t *testing.T) {
	// Regression: the earlier worst-case maxConcurrency*tokens KV bound flagged
	// saturation whenever the configured concurrency cap was high, even at
	// trivial load. The batch-aware model only lets KV *limit the batch*; with a
	// fast interconnect and tiny load it must not report saturation.
	mc := tinyDenseModel()
	// totalKVSlots = 100*16 = 1600; avgContext = 10 + 10/2 = 15; B_kv ≈ 106.
	// batch = min(1000, 106) = 106, well above 1 → no degenerate KV saturation.
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 10, AvgOutputTokens: 10, MaxConcurrency: 1000}
	entry := modelEntry{TP: 1, TotalKVBlocks: 100, BlockSizeTokens: 16, MaxRunningReqs: 256}

	sat, _ := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationNone {
		t.Errorf("high maxConcurrency at trivial load should not saturate, got %q", sat)
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

// decodeTPSAtBatch1 mirrors the production batch-aware decode ceiling at batch=1
// (MaxRunningReqs=1), so the margin-boundary tests track the real formula rather
// than a hardcoded constant.
func decodeTPSAtBatch1(mc *blisSim.ModelConfig, hc blisSim.HardwareCalib, inTok, outTok float64) float64 {
	avgContext := inTok + outTok/2.0
	tStep := (estimateWeightBytes(mc) + 1.0*avgContext*estimateKVBytesPerToken(mc)) / (hc.BwPeakTBs * 1e12)
	return 1.0 / tStep
}

func TestCheckSaturation_ExactlyAtCapacityIsNotSaturated(t *testing.T) {
	mc := tinyDenseModel()
	hc := loHWConfig()
	// demand at 97% of the batch-1 decode ceiling stays below the 0.98 margin.
	decodeTPS := decodeTPSAtBatch1(&mc, hc, 10, 1) // ≈1.5625 tok/s

	rps := float32(decodeTPS * 0.97)
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
	decodeTPS := decodeTPSAtBatch1(&mc, hc, 10, 1)

	// demand at 99% of capacity > 0.98 margin → should be saturated
	rps := float32(decodeTPS * 0.99)
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

// ---------------------------------------------------------------------------
// saturatedLatencyData — reported latency under pre-check saturation (issue #40)
// ---------------------------------------------------------------------------

func approxEq(a, b, tol float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func TestSaturatedLatencyData_Formula(t *testing.T) {
	// tStep=0.64s, outTokens=1, offeredRPS=3.0, maxRPS=1.5625
	//   itlMs=640, genMs=640, overload=3/1.5625=1.92
	//   ttft=1228.8, resp=1868.8, throughput=maxRPS=1.5625
	ad := saturatedLatencyData(0.64, 1, 3.0, 1.5625)

	if !approxEq(ad.AvgITL, 640, 0.1) {
		t.Errorf("AvgITL = %v, want ~640", ad.AvgITL)
	}
	if !approxEq(ad.AvgTTFT, 1228.8, 0.1) {
		t.Errorf("AvgTTFT = %v, want ~1228.8", ad.AvgTTFT)
	}
	if !approxEq(ad.AvgWaitTime, 1228.8, 0.1) {
		t.Errorf("AvgWaitTime = %v, want ~1228.8", ad.AvgWaitTime)
	}
	if !approxEq(ad.AvgRespTime, 1868.8, 0.1) {
		t.Errorf("AvgRespTime = %v, want ~1868.8", ad.AvgRespTime)
	}
	if !approxEq(ad.Throughput, 1.5625, 1e-4) || ad.Throughput != ad.MaxRPS {
		t.Errorf("Throughput/MaxRPS = %v/%v, want ~1.5625 and equal", ad.Throughput, ad.MaxRPS)
	}
}

func TestSaturatedLatencyData_InServiceTimeIsGenMs(t *testing.T) {
	// RespTime - WaitTime must equal the generation time (outTokens × ITL) so the
	// collector's Little's-Law occupancy stays physically sane.
	ad := saturatedLatencyData(0.64, 4, 3.0, 1.5625)
	inService := ad.AvgRespTime - ad.AvgWaitTime
	genMs := 4 * ad.AvgITL
	if !approxEq(inService, genMs, 0.1) {
		t.Errorf("RespTime-WaitTime = %v, want genMs = %v", inService, genMs)
	}
}

func TestSaturatedLatencyData_OverloadFloorIsOne(t *testing.T) {
	// At/below the capacity ceiling (RPS <= maxRPS) the overload factor floors at
	// 1, so TTFT = genMs (one generation time) rather than shrinking below it.
	ad := saturatedLatencyData(0.64, 2, 1.0, 1.5625) // RPS < maxRPS
	genMs := 2 * ad.AvgITL
	if !approxEq(ad.AvgTTFT, genMs, 0.1) {
		t.Errorf("AvgTTFT = %v, want genMs = %v (overload floored at 1)", ad.AvgTTFT, genMs)
	}
}

func TestSaturatedLatencyData_ZeroMaxRPSUsesUnitOverload(t *testing.T) {
	// KV-degenerate path: maxRPS=0 ⇒ overload=1, latency still non-zero.
	ad := saturatedLatencyData(0.872, 10, 0.5, 0)
	if ad.AvgITL <= 0 || ad.AvgTTFT <= 0 {
		t.Errorf("KV-degenerate latency must be non-zero, got ITL=%v TTFT=%v", ad.AvgITL, ad.AvgTTFT)
	}
	genMs := 10 * ad.AvgITL
	if !approxEq(ad.AvgTTFT, genMs, 0.1) {
		t.Errorf("AvgTTFT = %v, want genMs = %v with maxRPS=0", ad.AvgTTFT, genMs)
	}
}

func TestCheckSaturation_BandwidthSaturated_ReportsNonZeroLatency(t *testing.T) {
	mc := tinyDenseModel()
	pd := evaluator.ProblemData{RPS: 3.0, AvgInputTokens: 10, AvgOutputTokens: 1}
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	_, ad := checkSaturation(pd, &mc, loHWConfig(), entry)

	if ad.AvgITL <= 0 || ad.AvgTTFT <= 0 || ad.AvgRespTime <= 0 || ad.AvgWaitTime <= 0 {
		t.Errorf("saturated result must report non-zero latency, got %+v", ad)
	}
	if ad.Throughput <= 0 || ad.Throughput > pd.RPS {
		t.Errorf("Throughput = %v, want (0, RPS] under saturation", ad.Throughput)
	}
}

func TestCheckSaturation_TTFTMonotonicWithLoad_ITLFlat(t *testing.T) {
	mc := tinyDenseModel()
	entry := modelEntry{TP: 1, TotalKVBlocks: 10000, BlockSizeTokens: 16, MaxRunningReqs: 1}

	_, lo := checkSaturation(evaluator.ProblemData{RPS: 3.0, AvgInputTokens: 10, AvgOutputTokens: 1}, &mc, loHWConfig(), entry)
	_, hi := checkSaturation(evaluator.ProblemData{RPS: 5.0, AvgInputTokens: 10, AvgOutputTokens: 1}, &mc, loHWConfig(), entry)

	if hi.AvgTTFT <= lo.AvgTTFT {
		t.Errorf("TTFT should rise with load: RPS=3 gave %v, RPS=5 gave %v", lo.AvgTTFT, hi.AvgTTFT)
	}
	// ITL is the saturated batch's decode step time — independent of queued load.
	if !approxEq(hi.AvgITL, lo.AvgITL, 0.1) {
		t.Errorf("ITL should be flat with load: RPS=3 gave %v, RPS=5 gave %v", lo.AvgITL, hi.AvgITL)
	}
}

func TestCheckSaturation_KVDegenerate_ReportsNonZeroLatency(t *testing.T) {
	mc := tinyDenseModel()
	// Degenerate KV saturation at finite (low) bandwidth: totalKVSlots = 1*16 = 16;
	// avgContext = 20 + 10/2 = 25 > 16 → B_kv < 1. tStep at batch=1 is large.
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 20, AvgOutputTokens: 10}
	entry := modelEntry{TP: 1, TotalKVBlocks: 1, BlockSizeTokens: 16, MaxRunningReqs: 10}

	sat, ad := checkSaturation(pd, &mc, loHWConfig(), entry)

	if sat != evaluator.SaturationKV {
		t.Fatalf("saturation = %q, want %q", sat, evaluator.SaturationKV)
	}
	if ad.AvgITL <= 0 || ad.AvgTTFT <= 0 {
		t.Errorf("KV-degenerate result must report non-zero latency, got ITL=%v TTFT=%v", ad.AvgITL, ad.AvgTTFT)
	}
	if ad.MaxRPS != 0 {
		t.Errorf("MaxRPS = %v, want 0 (no stable rate) for degenerate KV saturation", ad.MaxRPS)
	}
}

func TestCheckSaturation_NeitherBottleneck(t *testing.T) {
	mc := tinyDenseModel()
	pd := evaluator.ProblemData{RPS: 0.001, AvgInputTokens: 1, AvgOutputTokens: 1}
	entry := modelEntry{
		TP: 1, TotalKVBlocks: 100000, BlockSizeTokens: 16, MaxRunningReqs: 1,
	}

	sat, ad := checkSaturation(pd, &mc, hiHWConfig(), entry)

	if sat != evaluator.SaturationNone {
		t.Errorf("well under capacity: saturation = %q, want none", sat)
	}
	if ad.MaxRPS != 0 {
		t.Errorf("maxRPS = %v, want 0 when not saturated", ad.MaxRPS)
	}
}
