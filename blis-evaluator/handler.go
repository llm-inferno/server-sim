package main

import (
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

		maxRunningReqs := entry.MaxRunningReqs
		if pd.MaxConcurrency > 0 {
			maxRunningReqs = int64(pd.MaxConcurrency)
		}

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
			PolicyConfig: blisSim.NewPolicyConfig("constant", entry.Scheduler),
		}

		// Pre-simulation saturation check: avoid running an expensive DES on
		// workloads that analytically exceed server capacity. The check uses
		// hardware and model parameters already loaded above and is independent
		// of the configured latency backend.
		if sat, maxRPS := checkSaturation(pd, modelConfig, hwConfig, entry); sat != "" {
			c.IndentedJSON(http.StatusOK, evaluator.AnalysisData{
				Saturation: sat,
				MaxRPS:     maxRPS,
			})
			return
		}

		// Build a single-client workload with exponential token length distributions.
		// Exponential requires only "mean", matching the ProblemData contract.
		spec := &workload.WorkloadSpec{
			AggregateRate: float64(pd.RPS),
			Seed:          entry.Seed,
			Clients: []workload.ClientSpec{
				{
					ID:           "client-0",
					RateFraction: 1.0,
					Arrival:      workload.ArrivalSpec{Process: "poisson"},
					InputDist: workload.DistSpec{
						Type:   "exponential",
						Params: map[string]float64{"mean": float64(pd.AvgInputTokens)},
					},
					OutputDist: workload.DistSpec{
						Type:   "exponential",
						Params: map[string]float64{"mean": float64(pd.AvgOutputTokens)},
					},
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

// checkSaturation performs an analytical pre-simulation overload check using
// parameters already loaded in the handler. It returns the saturation reason
// and a derived MaxRPS if the offered workload exceeds server capacity, or
// ("", 0) if the workload appears sustainable.
//
// Two independent bottlenecks are checked:
//
//  1. Decode memory bandwidth: each decode step must stream model weights once.
//     The bandwidth ceiling on decode throughput is:
//     decodeCapacityTPS = (BwPeakTBs × TP × 1e12) / totalWeightBytes
//     Saturated if RPS × AvgOutputTokens > decodeCapacityTPS × saturationMargin.
//
//  2. KV cache capacity: the KV slots must fit all in-flight token contexts.
//     Saturated if MaxRunningReqs × avgSeqLen > TotalKVBlocks × BlockSize × saturationMargin.
//
// See docs/saturation-detection.md and docs/blis-overload-detection.md for details.
func checkSaturation(pd evaluator.ProblemData, mc *blisSim.ModelConfig, hc blisSim.HardwareCalib, entry modelEntry) (saturation string, maxRPS float32) {
	tp := entry.TP
	if tp <= 0 {
		tp = 1
	}

	// --- Bottleneck A: decode memory bandwidth ---
	weightBytes := estimateWeightBytes(mc)
	if weightBytes > 0 && hc.BwPeakTBs > 0 {
		decodeCapacityTPS := (hc.BwPeakTBs * float64(tp) * 1e12) / weightBytes
		demandTPS := float64(pd.RPS) * float64(pd.AvgOutputTokens)
		if demandTPS > decodeCapacityTPS*saturationMargin {
			derivedMaxRPS := float32(decodeCapacityTPS / float64(pd.AvgOutputTokens))
			return evaluator.SaturationBandwidth, derivedMaxRPS
		}
	}

	// --- Bottleneck B: KV cache capacity ---
	totalKVSlots := entry.TotalKVBlocks * entry.BlockSizeTokens
	maxRunningReqs := entry.MaxRunningReqs
	if pd.MaxConcurrency > 0 {
		maxRunningReqs = int64(pd.MaxConcurrency)
	}
	avgTokensPerReq := float64(pd.AvgInputTokens) + float64(pd.AvgOutputTokens)
	if totalKVSlots > 0 && avgTokensPerReq > 0 && maxRunningReqs > 0 {
		concurrentKVTokens := float64(maxRunningReqs) * avgTokensPerReq
		if concurrentKVTokens > float64(totalKVSlots)*saturationMargin {
			return evaluator.SaturationKV, 0
		}
	}

	return evaluator.SaturationNone, 0
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
