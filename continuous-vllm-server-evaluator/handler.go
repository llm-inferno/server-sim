package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// roundTokenAvg rejects NaN/Inf and values < 1, otherwise rounds to the
// nearest int (≥ 1). Sampler construction would silently degrade a fractional
// or negative avg into a degenerate workload, masking upstream scale or unit
// bugs in the request — refuse it at the boundary instead.
func roundTokenAvg(name string, v float32) (int, error) {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s must be finite, got %v", name, v)
	}
	if f < 1 {
		return 0, fmt.Errorf("%s must be >= 1, got %v", name, v)
	}
	return int(math.Round(f)), nil
}

// solve reconfigures the live arrival loop from pd and returns metrics over the
// trailing window. Status is an HTTP code for the caller; 200 on success.
func (g *generator) solve(ctx context.Context, pd evaluator.ProblemData) (evaluator.AnalysisData, int, error) {
	sc, ok := g.lookup[pd.Accelerator+"|"+pd.Model]
	if !ok {
		return evaluator.AnalysisData{}, http.StatusBadRequest,
			fmt.Errorf("no config for %s|%s", pd.Accelerator, pd.Model)
	}
	base := g.baseURL()
	if base == "" {
		return evaluator.AnalysisData{}, http.StatusServiceUnavailable,
			fmt.Errorf("paired vLLM not resolved yet")
	}

	inAvg, err := roundTokenAvg("avgInputTokens", pd.AvgInputTokens)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	outAvg, err := roundTokenAvg("avgOutputTokens", pd.AvgOutputTokens)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	inSampler, err := newSampler(sc.InputTokenDistribution, inAvg)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	outSampler, err := newSampler(sc.OutputTokenDistribution, outAvg)
	if err != nil {
		return evaluator.AnalysisData{}, http.StatusBadRequest, err
	}
	conc := evaluator.ResolveMaxConcurrency(pd.MaxConcurrency, sc.DefaultConcurrency, "continuous-vllm-server")
	windowSec := float64(sc.TrailingWindowSec)

	// Swap the live config and resize the limiter — the running loop picks these
	// up on its next arrival. This is the "reconfigure, keep running" step.
	g.live.Store(&liveConfig{
		rps:         float64(pd.RPS),
		concurrency: conc,
		inSampler:   inSampler,
		outSampler:  outSampler,
		ignoreEOS:   sc.IgnoreEOS,
		servedModel: sc.VLLMServedModelName,
		queueMetric: sc.QueueTimeMetric,
		minSamples:  sc.MinSamples,
		windowSec:   windowSec,
	})
	g.lim.setLimit(conc)

	// Scrape /metrics now and record it; queue/inference means come from the
	// trailing delta across the scrape ring.
	now := time.Now()
	if m, serr := g.scrape(ctx, base+"/metrics", sc.QueueTimeMetric); serr == nil {
		g.scrapes.add(m, now)
	} else {
		log.Printf("solve: scrape /metrics failed (queue means degrade to 0): %v", serr)
	}
	queueMeanSec, inferMeanSec := g.scrapes.trailingMeans(now, time.Duration(windowSec)*time.Second)

	samples := g.ring.snapshot(now, time.Duration(windowSec)*time.Second)
	completed := 0
	for _, s := range samples {
		if !s.Failed {
			completed++
		}
	}
	if completed < sc.MinSamples {
		return evaluator.AnalysisData{}, http.StatusInternalServerError,
			fmt.Errorf("insufficient samples: need %d, got %d", sc.MinSamples, completed)
	}

	ad := aggregateTrailing(samples, pd.RPS, windowSec, queueMeanSec)
	ad.Saturation = detectSaturationTrailing(samples, sc.MinSamples, queueMeanSec, inferMeanSec)
	return ad, http.StatusOK, nil
}

func solveHandler(g *generator) gin.HandlerFunc {
	return func(c *gin.Context) {
		var pd evaluator.ProblemData
		if err := c.ShouldBindJSON(&pd); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad request: " + err.Error()})
			return
		}
		ad, status, err := g.solve(c.Request.Context(), pd)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, ad)
	}
}
