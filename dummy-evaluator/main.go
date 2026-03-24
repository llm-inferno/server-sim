// dummy-evaluator is a minimal standalone service that implements the evaluator
// /solve API with plausible hardcoded responses. It is used to validate the
// full server-sim async job flow without requiring a real evaluator backend.
package main

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func main() {
	port := 8081
	if v := os.Getenv("DUMMY_EVALUATOR_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	r := gin.Default()
	r.POST("/solve", handleSolve)
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		panic(err)
	}
}

// handleSolve returns plausible hardcoded metrics scaled loosely by input RPS.
func handleSolve(c *gin.Context) {
	var pd evaluator.ProblemData
	if err := c.ShouldBindJSON(&pd); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	// Simple scaling: higher RPS → higher latency, lower throughput headroom.
	// These values are illustrative, not analytically derived.
	loadFactor := float32(math.Max(1.0, float64(pd.RPS)/5.0))

	ad := evaluator.AnalysisData{
		Throughput:   pd.RPS * 0.95,
		AvgRespTime:  5000 * loadFactor,
		AvgWaitTime:  20 * loadFactor,
		AvgNumInServ: float32(pd.MaxBatchSize) * 0.6 * loadFactor,
		AvgTTFT:      50 * loadFactor,
		AvgITL:       15 * loadFactor,
		MaxRPS:       float32(pd.MaxBatchSize) * 0.08,
	}

	c.IndentedJSON(http.StatusOK, ad)
}
