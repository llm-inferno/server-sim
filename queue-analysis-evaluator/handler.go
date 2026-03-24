package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	qaAnalyzer "github.com/llm-inferno/queue-analysis/pkg/analyzer"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

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

		// MaxConcurrency from request overrides the model-data default when provided.
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
			Throughput:   metrics.Throughput,
			AvgRespTime:  metrics.AvgRespTime,
			AvgWaitTime:  metrics.AvgWaitTime,
			AvgTTFT:      metrics.AvgTTFT,
			AvgITL:       metrics.AvgTokenTime, // AvgTokenTime == inter-token latency
			MaxRPS:       metrics.MaxRate,
		}
		c.IndentedJSON(http.StatusOK, ad)
	}
}
