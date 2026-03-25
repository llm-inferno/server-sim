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

		ad := extractMetrics(cs.AggregatedMetrics())
		c.IndentedJSON(http.StatusOK, ad)
	}
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
	return evaluator.AnalysisData{
		Throughput:   float32(responsesPerSec),
		AvgRespTime:  float32(blisSim.CalculateMean(mapVals(m.RequestE2Es))),
		AvgWaitTime:  float32(blisSim.CalculateMean(mapVals(m.RequestSchedulingDelays))),
		AvgTTFT:      float32(blisSim.CalculateMean(mapVals(m.RequestTTFTs))),
		AvgITL:       float32(blisSim.CalculateMean(m.AllITLs)),
		// MaxRPS: approximated as achieved throughput — the simulation runs at the
		// requested RPS; if the system is stable, throughput ≈ max stable rate.
		MaxRPS: float32(responsesPerSec),
	}
}
