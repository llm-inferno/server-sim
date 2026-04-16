package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	qaAnalyzer "github.com/llm-inferno/queue-analysis/pkg/analyzer"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// saturationMargin is the fraction of MaxRate at which the system is considered
// saturated. A 2% headroom accounts for model approximation errors.
const saturationMargin = 0.98

// solveHandler returns a Gin handler that resolves accelerator+model to
// queue-analysis parameters, runs the analytical model, and returns metrics.
func solveHandler(lookup map[string]serverConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var pd evaluator.ProblemData
		if err := c.ShouldBindJSON(&pd); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}

		key := pd.Accelerator + "|" + pd.Model
		sc, ok := lookup[key]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "unknown accelerator/model combination: " + pd.Accelerator + " / " + pd.Model,
			})
			return
		}

		// Use pd.MaxConcurrency as the primary source; fall back to sc.MaxBatchSize if absent or invalid.
		maxBatchSize := sc.MaxBatchSize
		if pd.MaxConcurrency > 0 {
			maxBatchSize = pd.MaxConcurrency
		}

		config := &qaAnalyzer.Configuration{
			MaxBatchSize: maxBatchSize,
			MaxQueueSize: sc.MaxQueueSize,
			ServiceParms: &qaAnalyzer.ServiceParms{
				Alpha: sc.Alpha,
				Beta:  sc.Beta,
				Gamma: sc.Gamma,
			},
		}
		requestSize := &qaAnalyzer.RequestSize{
			AvgInputTokens:  pd.AvgInputTokens,
			AvgOutputTokens: pd.AvgOutputTokens,
		}

		qa, err := qaAnalyzer.NewLLMQueueAnalyzer(config, requestSize)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create analyzer: " + err.Error()})
			return
		}

		metrics, err := qa.Analyze(pd.RPS)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "analyze: " + err.Error()})
			return
		}

		ad := evaluator.AnalysisData{
			Throughput:  metrics.Throughput,
			AvgRespTime: metrics.AvgRespTime,
			AvgWaitTime: metrics.AvgWaitTime,
			AvgTTFT:     metrics.AvgTTFT,
			AvgITL:      metrics.AvgTokenTime, // AvgTokenTime == inter-token latency
			MaxRPS:      metrics.MaxRate,
		}

		// Saturation check: offered rate exceeds maximum stable rate (with 2% margin).
		// Metrics are left populated — they reflect degraded-state behaviour under
		// overload. Consumers MUST check Saturation before treating them as reliable.
		if metrics.MaxRate > 0 && float64(pd.RPS) > float64(metrics.MaxRate)*saturationMargin {
			ad.Saturation = evaluator.SaturationOverload
		}

		c.IndentedJSON(http.StatusOK, ad)
	}
}
