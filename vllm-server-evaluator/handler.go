package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// handlerState is the shared state injected into solveHandler.
type handlerState struct {
	Lookup map[string]serverConfig

	// Pairing is updated by the background resolver goroutine in main.go.
	// Tests populate it directly via Pairing.Store(...) before the handler is invoked.
	Pairing atomic.Pointer[pairingState]

	// BaseURLOverride is a test hook. Production leaves this empty and the
	// handler constructs the URL from Pairing.VLLMPodIP:Pairing.VLLMPort.
	BaseURLOverride string

	// vllmMu serializes /solve calls per (vllm) endpoint. v1: only one paired
	// vLLM per evaluator, so a single mutex is enough.
	vllmMu sync.Mutex
}

func solveHandler(st *handlerState) gin.HandlerFunc {
	return func(c *gin.Context) {
		var pd evaluator.ProblemData
		if err := c.ShouldBindJSON(&pd); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}

		ps := st.Pairing.Load()
		if (ps == nil || ps.VLLMPodIP == "") && st.BaseURLOverride == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vllm pairing not ready"})
			return
		}

		key := pd.Accelerator + "|" + pd.Model
		sc, ok := st.Lookup[key]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "unknown accelerator/model combination: " + pd.Accelerator + " / " + pd.Model,
			})
			return
		}

		baseURL := st.BaseURLOverride
		if baseURL == "" {
			baseURL = fmt.Sprintf("http://%s:%d", ps.VLLMPodIP, ps.VLLMPort)
		}

		st.vllmMu.Lock()
		defer st.vllmMu.Unlock()

		// 1. Validate served-model.
		if err := verifyServedModel(c.Request.Context(), baseURL, sc.VLLMServedModelName); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 2. Scrape /metrics at window start.
		startScrape, err := scrapeMetrics(c.Request.Context(), baseURL+"/metrics", sc.QueueTimeMetric)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scrape metrics: " + err.Error()})
			return
		}

		// 3. Build per-request token samplers from the configured
		//    distribution kinds and request averages.
		inSampler, err := newSampler(sc.InputTokenDistribution, int(pd.AvgInputTokens))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "input sampler: " + err.Error()})
			return
		}
		outSampler, err := newSampler(sc.OutputTokenDistribution, int(pd.AvgOutputTokens))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "output sampler: " + err.Error()})
			return
		}

		// 4. Run the measurement window.
		wp := windowParams{
			BaseURL:       baseURL,
			Model:         sc.VLLMServedModelName,
			InputSampler:  inSampler,
			OutputSampler: outSampler,
			IgnoreEOS:     sc.IgnoreEOS,
			RPS:           float64(pd.RPS),
			WarmupSec:     sc.WarmupSec,
			MinWindowSec:  sc.MinWindowSec,
			MaxWindowSec:  sc.MaxWindowSec,
			TargetSamples: sc.TargetSamples,
			Concurrency:   pd.MaxConcurrency, // 0 → default 64 inside runWindow
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(sc.WarmupSec+sc.MaxWindowSec+10)*time.Second)
		defer cancel()
		res, err := runWindow(ctx, wp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "window: " + err.Error()})
			return
		}
		res.ScrapeAtStart = startScrape

		// 5. Scrape /metrics at window end.
		endScrape, err := scrapeMetrics(c.Request.Context(), baseURL+"/metrics", sc.QueueTimeMetric)
		if err == nil {
			res.ScrapeAtEnd = endScrape
		}

		// 6. Insufficient-samples guard.
		completed := 0
		for _, s := range res.Samples {
			if !s.Failed {
				completed++
			}
		}
		if completed < sc.MinSamples {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": fmt.Sprintf("insufficient samples: need %d, got %d", sc.MinSamples, completed),
			})
			return
		}

		// 7. Aggregate + saturation.
		ad := aggregate(*res, pd.RPS)
		ad.Saturation = detectSaturation(*res, sc.MinSamples)

		c.IndentedJSON(http.StatusOK, ad)
	}
}

// verifyServedModel hits /v1/models and confirms the desired model is listed.
func verifyServedModel(ctx context.Context, baseURL, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("verify served model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("verify served model: /v1/models status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode /v1/models: %w", err)
	}
	for _, m := range body.Data {
		if m.ID == want {
			return nil
		}
	}
	got := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		got = append(got, m.ID)
	}
	return fmt.Errorf("vllm serves %v, requested %s", got, want)
}
