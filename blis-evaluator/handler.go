package main

import (
	"math"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	blisSim "github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/cluster"
	"github.com/inference-sim/inference-sim/sim/latency"
	"github.com/inference-sim/inference-sim/sim/workload"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// saturationMargin is the fraction of capacity at which we consider the system
// saturated. A 2% headroom (0.98) accounts for estimation inaccuracy in the
// analytical formulas (approximate param counts, especially for MoE models).
const saturationMargin = 0.98

// solveHandler returns a Gin handler that maps ProblemData to BLIS simulation
// parameters, runs a DES simulation, and returns AnalysisData metrics.
func solveHandler(lookup map[string]modelEntry, backend string) gin.HandlerFunc {
	globalHWConfigFile := os.Getenv("HW_CONFIG_FILE")
	if globalHWConfigFile == "" {
		globalHWConfigFile = "hardware_config.json"
	}

	return func(c *gin.Context) {
		var pd evaluator.ProblemData
		if err := c.ShouldBindJSON(&pd); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}

		key := modelKey(pd.Accelerator, pd.Model)
		entry, ok := lookup[key]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "unknown accelerator/model combination: " + pd.Accelerator + " / " + pd.Model,
			})
			return
		}

		modelConfig, err := latency.GetModelConfig(entry.HFConfigPath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "load model config: " + err.Error()})
			return
		}

		hwConfigFile := entry.HWConfigPath
		if hwConfigFile == "" {
			hwConfigFile = globalHWConfigFile
		}
		hwConfig, err := latency.GetHWConfig(hwConfigFile, entry.GPU)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "load hardware config: " + err.Error()})
			return
		}

		maxRunningReqs := effectiveMaxRunningReqs(pd, entry)

		simCfg := blisSim.SimConfig{
			Horizon: entry.SimulationHorizon,
			Seed:    entry.Seed,
			KVCacheConfig: blisSim.NewKVCacheConfig(
				entry.TotalKVBlocks, entry.BlockSizeTokens,
				0, 0.0, 0.0, 0,
			),
			BatchConfig: blisSim.NewBatchConfig(maxRunningReqs, entry.MaxScheduledTokens, 0),
			LatencyCoeffs: blisSim.NewLatencyCoeffs(
				entry.BetaCoeffs,
				entry.AlphaCoeffs,
			),
			ModelHardwareConfig: blisSim.NewModelHardwareConfig(
				*modelConfig, hwConfig,
				pd.Model, entry.GPU, entry.TP, backend, entry.MaxModelLen,
			),
			PolicyConfig: blisSim.NewPolicyConfig(entry.Scheduler, "fcfs"),
		}

		// Pre-simulation saturation check: avoid running an expensive DES on
		// workloads that analytically exceed server capacity. The check uses
		// hardware and model parameters already loaded above and is independent
		// of the configured latency backend. When saturated it returns a result
		// pre-populated with a large, load-monotonic latency (not zeros) so the
		// downstream dashboard renders saturation distinctly from a "no data"
		// dropout — see docs/blis-saturated-latency.md (issue #40).
		if sat, ad := checkSaturation(pd, modelConfig, hwConfig, entry); sat != "" {
			ad.Saturation = sat
			c.IndentedJSON(http.StatusOK, ad)
			return
		}

		// Build a single-client workload whose token-length distributions are
		// synthesized by tokenDist from the per-entry tokenDist config (type,
		// cov, min) and the MaxModelLen sum-split. A nil config defaults to a
		// constant (fixed-length) distribution.
		maxModelLen := float64(entry.MaxModelLen)
		inMean := float64(pd.AvgInputTokens)
		outMean := float64(pd.AvgOutputTokens)
		spec := &workload.WorkloadSpec{
			AggregateRate: float64(pd.RPS),
			Seed:          entry.Seed,
			Clients: []workload.ClientSpec{
				{
					ID:           "client-0",
					RateFraction: 1.0,
					Arrival:      workload.ArrivalSpec{Process: "poisson"},
					InputDist:    tokenDist(entry.TokenDist, inMean, outMean, maxModelLen),
					OutputDist:   tokenDist(entry.TokenDist, outMean, inMean, maxModelLen),
				},
			},
		}

		requests, err := workload.GenerateRequests(spec, entry.SimulationHorizon, entry.NumRequests)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "generate workload: " + err.Error()})
			return
		}

		cs := cluster.NewClusterSimulator(
			cluster.DeploymentConfig{SimConfig: simCfg, NumInstances: 1},
			requests,
			nil,
		)
		if err := cs.Run(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "run simulation: " + err.Error()})
			return
		}

		m := cs.AggregatedMetrics()
		ad := extractMetrics(m)

		// Post-sim safety net: if the DES ran but overload indicators are present,
		// flag the result so consumers know the metrics reflect degraded-state
		// behaviour rather than stable-throughput operation.
		if m.StillQueued > 0 || m.KVAllocationFailures > 0 || m.TimedOutRequests > 0 {
			ad.Saturation = evaluator.SaturationOverload
		}

		c.IndentedJSON(http.StatusOK, ad)
	}
}

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
		max := sumSplitMax(mean, otherMean, maxModelLen)
		return workload.DistSpec{
			Type: "gaussian",
			Params: map[string]float64{
				"mean":    mean,
				"std_dev": cfg.Cov * mean,
				"min":     clampMin(cfg.Min, max),
				"max":     max,
			},
		}
	case "lognormal":
		sigma := math.Sqrt(math.Log(1 + cfg.Cov*cfg.Cov))
		mu := math.Log(mean) - sigma*sigma/2
		max := sumSplitMax(mean, otherMean, maxModelLen)
		return workload.DistSpec{
			Type: "lognormal",
			Params: map[string]float64{
				"mu":    mu,
				"sigma": sigma,
				"min":   clampMin(cfg.Min, max),
				"max":   max,
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

// clampMin keeps the configured clamp floor from exceeding this dimension's
// dynamic ceiling (max). The floor is best-effort against the request-derived
// sum-split cap, which config validation cannot know: when a skewed mean ratio
// shrinks this dimension's share below the configured min, we cap min at max so
// the sampler sees a coherent [min, max] band instead of an inverted one.
func clampMin(min, max float64) float64 {
	if min > max {
		return max
	}
	return min
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

// effectiveMaxRunningReqs returns the running-request cap used for both the
// pre-simulation saturation check and the DES batch config: the request's
// MaxConcurrency when positive, else the per-model maxRunningReqs (validated
// > 0 at config load). Both call sites MUST use this so the saturation gate and
// the simulation run against the same cap.
//
// blis is intentionally exempt from the shared evaluator.DefaultMaxConcurrency
// backstop: the configured fallback is always positive, so a
// hardware-inappropriate guess is never substituted for a KV-bound simulation.
func effectiveMaxRunningReqs(pd evaluator.ProblemData, entry modelEntry) int64 {
	if pd.MaxConcurrency > 0 {
		return int64(pd.MaxConcurrency)
	}
	return entry.MaxRunningReqs
}

// checkSaturation performs an analytical pre-simulation overload check using
// parameters already loaded in the handler. It returns the saturation reason
// and an AnalysisData pre-populated with the derived MaxRPS and a large,
// load-monotonic saturated latency (see saturatedLatencyData) if the offered
// workload exceeds server capacity, or ("", zero AnalysisData) if the workload
// appears sustainable. The caller stamps the returned Saturation field.
//
// The model is batch-aware: a real vLLM server decodes a whole running batch per
// forward pass, streaming the model weights once and reading the KV cache of every
// in-flight context. The running batch B is bounded by KV capacity:
//
//	avgContext = L_in + L_out/2                 (mean KV tokens per in-flight request)
//	B_kv       = TotalKVSlots / avgContext      (avg-length contexts that fit)
//	B          = min(maxRunningReqs, B_kv)
//
// Per decode step the engine streams weights once plus the KV of all B contexts and
// emits B new tokens, so the decode token-throughput ceiling is:
//
//	t_step    = (weightBytes + B*avgContext*kvBytesPerToken) / (BW*TP)
//	decodeTPS = B / t_step
//
// Saturated (bandwidth) if RPS*L_out > decodeTPS*saturationMargin, with
// MaxRPS = decodeTPS/L_out. A degenerate KV saturation is reported when a single
// average request's context does not fit in KV at all (B_kv < 1).
//
// This batch-aware form replaces an earlier bound that compared aggregate demand
// against the batch-size-1 weight-streaming rate (ignoring batching) and used a
// worst-case maxConcurrency*tokens KV occupancy; both were far too conservative
// for batched serving (e.g. they vetoed Qwen2.5-14B/H100 at ~0.22 RPS). See
// docs/saturation-detection.md and docs/blis-overload-detection.md.
func checkSaturation(pd evaluator.ProblemData, mc *blisSim.ModelConfig, hc blisSim.HardwareCalib, entry modelEntry) (saturation string, ad evaluator.AnalysisData) {
	tp := entry.TP
	if tp <= 0 {
		tp = 1
	}

	outTokens := float64(pd.AvgOutputTokens)
	avgContext := float64(pd.AvgInputTokens) + outTokens/2.0
	totalKVSlots := float64(entry.TotalKVBlocks * entry.BlockSizeTokens)
	batch := float64(effectiveMaxRunningReqs(pd, entry))

	weightBytes := estimateWeightBytes(mc)
	kvBytesPerToken := estimateKVBytesPerToken(mc)
	bwBytesPerSec := hc.BwPeakTBs * float64(tp) * 1e12

	// KV capacity limits the running batch. If a single average request's context
	// does not fit, the workload is unservable (degenerate KV saturation): no
	// stable rate exists (MaxRPS=0), so we report the decode step at batch=1 with
	// no overload factor — a large finite latency at real bandwidth, not a zero.
	if totalKVSlots > 0 && avgContext > 0 {
		bKV := totalKVSlots / avgContext
		if bKV < 1.0 {
			// Guard the same finite-bandwidth preconditions as the bandwidth branch
			// so tStep stays finite (a zero/absent bandwidth would yield +Inf, which
			// is not JSON-encodable); fall back to MaxRPS-only when they don't hold.
			ad := evaluator.AnalysisData{}
			if weightBytes > 0 && bwBytesPerSec > 0 {
				tStep := decodeStep(weightBytes, kvBytesPerToken, bwBytesPerSec, 1.0, avgContext)
				ad = saturatedLatencyData(tStep, outTokens, float64(pd.RPS), 0)
			}
			return evaluator.SaturationKV, ad
		}
		if bKV < batch {
			batch = bKV
		}
	}

	// Decode memory-bandwidth ceiling at the (KV-limited) running batch.
	if outTokens > 0 && weightBytes > 0 && bwBytesPerSec > 0 && batch > 0 {
		// tStep > 0 is guaranteed: weightBytes > 0 and bwBytesPerSec > 0.
		tStep := decodeStep(weightBytes, kvBytesPerToken, bwBytesPerSec, batch, avgContext)
		decodeTPS := batch / tStep
		demandTPS := float64(pd.RPS) * outTokens
		if demandTPS > decodeTPS*saturationMargin {
			maxRPS := float32(decodeTPS / outTokens)
			return evaluator.SaturationBandwidth, saturatedLatencyData(tStep, outTokens, float64(pd.RPS), maxRPS)
		}
	}

	return evaluator.SaturationNone, evaluator.AnalysisData{}
}

// decodeStep returns the decode step time (seconds) for a running batch: the
// engine streams the model weights once and reads the KV of all `batch`
// in-flight contexts (avgContext tokens each) per forward pass.
func decodeStep(weightBytes, kvBytesPerToken, bwBytesPerSec, batch, avgContext float64) float64 {
	return (weightBytes + batch*avgContext*kvBytesPerToken) / bwBytesPerSec
}

// saturatedLatencyData builds the latency fields reported for a saturated
// pre-check result, so consumers see a large, load-monotonic latency instead of
// a misleading zero (issue #40). Derivation and rationale: see
// docs/blis-saturated-latency.md.
//
//	AvgITL      = tStep                      (per-token latency of the saturated batch)
//	genMs       = outTokens × AvgITL         (in-service generation time)
//	overload    = max(1, RPS / maxRPS)       (1 when maxRPS<=0; grows past the ceiling)
//	AvgTTFT     = genMs × overload           (queueing-dominated; rises with load)
//	AvgWaitTime = AvgTTFT
//	AvgRespTime = AvgTTFT + genMs            (wait + generation; in-service = genMs)
//	Throughput  = maxRPS                     (saturated goodput ceiling; <= RPS)
func saturatedLatencyData(tStep, outTokens, offeredRPS float64, maxRPS float32) evaluator.AnalysisData {
	itlMs := tStep * 1000.0
	genMs := outTokens * itlMs

	overload := 1.0
	if maxRPS > 0 {
		if r := offeredRPS / float64(maxRPS); r > 1.0 {
			overload = r
		}
	}
	ttftMs := genMs * overload

	return evaluator.AnalysisData{
		Throughput:  maxRPS,
		AvgRespTime: float32(ttftMs + genMs),
		AvgWaitTime: float32(ttftMs),
		AvgTTFT:     float32(ttftMs),
		AvgITL:      float32(itlMs),
		MaxRPS:      maxRPS,
	}
}

// estimateKVBytesPerToken returns the per-token KV cache size in bytes — keys and
// values across all layers — using the same dtype assumption as the weight
// estimate. For grouped-query attention this uses the KV head count, not the
// (larger) attention head count.
func estimateKVBytesPerToken(mc *blisSim.ModelConfig) float64 {
	numKVHeads := mc.NumKVHeads
	if numKVHeads == 0 {
		numKVHeads = mc.NumHeads
	}
	var headDim int64
	if mc.NumHeads > 0 {
		headDim = int64(mc.HiddenDim) / int64(mc.NumHeads)
	}
	// 2 = K and V tensors.
	return 2.0 * float64(numKVHeads) * float64(headDim) * float64(mc.NumLayers) * mc.EffectiveWeightBytesPerParam()
}

// estimateWeightBytes returns a conservative estimate of total model weight
// bytes (all parameters × effective bytes per param). It replicates the core
// formula from the unexported computeModelWeightBytes in the inference-sim
// library. For MoE models all routed experts are counted (no nEff reduction),
// which overestimates weight memory and makes the saturation check conservative.
func estimateWeightBytes(mc *blisSim.ModelConfig) float64 {
	h := int64(mc.HiddenDim)
	nLayers := int64(mc.NumLayers)
	vocab := int64(mc.VocabSize)
	inter := int64(mc.IntermediateDim)
	if inter == 0 {
		inter = 4 * h
	}

	numKVHeads := mc.NumKVHeads
	if numKVHeads == 0 {
		numKVHeads = mc.NumHeads
	}
	headDim := h / int64(mc.NumHeads)
	kvDim := int64(numKVHeads) * headDim

	// Embeddings: vocab × hidden
	embeddings := vocab * h

	// Attention per layer: Q + K + V + O projections
	attnPerLayer := h*(h+2*kvDim) + h*h

	// MLP per layer (3 matrices: gate + up + down for SwiGLU)
	var mlpPerLayer int64
	if mc.NumLocalExperts > 1 {
		// MoE: all routed experts counted (conservative)
		expertFFNDim := inter
		if mc.MoEExpertFFNDim > 0 {
			expertFFNDim = int64(mc.MoEExpertFFNDim)
		}
		mlpPerLayer = 3 * h * expertFFNDim * int64(mc.NumLocalExperts)
	} else {
		mlpPerLayer = 3 * h * inter
	}

	// Layer norms: 2 per layer
	normsPerLayer := 2 * h

	// lm_head + final norm (include lm_head conservatively; no tie check)
	lmHead := vocab * h
	finalNorm := h

	totalParams := embeddings + nLayers*(attnPerLayer+mlpPerLayer+normsPerLayer) + lmHead + finalNorm
	return float64(totalParams) * mc.EffectiveWeightBytesPerParam()
}

// mapVals extracts the values from a map[string]T into a []float64 slice.
func mapVals[T float64 | int64](m map[string]T) []float64 {
	s := make([]float64, 0, len(m))
	for _, v := range m {
		s = append(s, float64(v))
	}
	return s
}

// extractMetrics computes AnalysisData directly from the aggregated *sim.Metrics,
// replicating the calculations in sim.Metrics.SaveResults without writing to files.
func extractMetrics(m *blisSim.Metrics) evaluator.AnalysisData {
	vllmRuntime := float64(m.SimEndedTime) / 1e6 // ticks (µs) → seconds
	var responsesPerSec float64
	if m.CompletedRequests > 0 && vllmRuntime > 0 {
		responsesPerSec = float64(m.CompletedRequests) / vllmRuntime
	}

	// CalculateMean divides by 1000 to convert µs → ms.
	// MaxRPS is 0 here: the DES runs at the requested RPS and does not compute a
	// capacity limit. When saturation is detected analytically (pre-sim), MaxRPS
	// is derived from the bandwidth ceiling and returned directly without running
	// the DES (see checkSaturation).
	return evaluator.AnalysisData{
		Throughput:  float32(responsesPerSec),
		AvgRespTime: float32(blisSim.CalculateMean(mapVals(m.RequestE2Es))),
		AvgWaitTime: float32(blisSim.CalculateMean(mapVals(m.RequestSchedulingDelays))),
		AvgTTFT:     float32(blisSim.CalculateMean(mapVals(m.RequestTTFTs))),
		AvgITL:      float32(blisSim.CalculateMean(m.AllITLs)),
		MaxRPS:      0,
	}
}
