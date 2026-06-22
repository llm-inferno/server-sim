package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func newTestGen() *generator {
	g := newGenerator(map[string]serverConfig{
		"H100|m": {
			VLLMServedModelName: "m",
			MinSamples:          5,
			QueueTimeMetric:     "vllm:request_queue_time_seconds",
			TrailingWindowSec:   30,
			DefaultConcurrency:  64,
		},
	})
	g.baseURLOverride = "http://fake"
	g.scrape = func(ctx context.Context, url, q string) (metricsScrape, error) {
		return metricsScrape{}, nil // no queue time in the unit test
	}
	return g
}

func TestSolve_ReconfiguresLiveConfig(t *testing.T) {
	g := newTestGen()
	pd := evaluator.ProblemData{RPS: 12, MaxConcurrency: 32, AvgInputTokens: 16, AvgOutputTokens: 8, Accelerator: "H100", Model: "m"}
	// Pre-seed the ring so we clear MinSamples.
	for i := 0; i < 6; i++ {
		g.ring.add(sample{TTFT: 50 * time.Millisecond, ResponseTime: 60 * time.Millisecond}, time.Now())
	}
	_, status, err := g.solve(context.Background(), pd)
	if err != nil || status != http.StatusOK {
		t.Fatalf("solve: status=%d err=%v", status, err)
	}
	cfg := g.live.Load()
	if cfg == nil || cfg.rps != 12 || cfg.concurrency != 32 {
		t.Fatalf("live config not swapped: %+v", cfg)
	}
	if g.lim.currentLimit() != 32 {
		t.Fatalf("limiter limit = %d, want 32", g.lim.currentLimit())
	}
}

func TestSolve_InsufficientSamplesReturns500(t *testing.T) {
	g := newTestGen()
	pd := evaluator.ProblemData{RPS: 12, AvgInputTokens: 16, AvgOutputTokens: 8, Accelerator: "H100", Model: "m"}
	_, status, err := g.solve(context.Background(), pd) // ring empty
	if status != http.StatusInternalServerError || err == nil {
		t.Fatalf("want 500 + error on empty ring, got status=%d err=%v", status, err)
	}
	// Assert the live config was swapped and limiter was resized even on the 500 path
	cfg := g.live.Load()
	if cfg == nil || cfg.rps != 12 {
		t.Errorf("live config must be reconfigured even on the 500 path: %+v", cfg)
	}
	if g.lim.currentLimit() != 64 {
		t.Errorf("limiter must be resized even on the 500 path: got %d, want 64", g.lim.currentLimit())
	}
}

func TestSolveHandler_HTTP(t *testing.T) {
	g := newTestGen()
	for i := 0; i < 6; i++ {
		g.ring.add(sample{TTFT: 50 * time.Millisecond, ITLs: []time.Duration{10 * time.Millisecond}, ResponseTime: 60 * time.Millisecond}, time.Now())
	}
	r := gin.New()
	r.POST("/solve", solveHandler(g))
	pd := evaluator.ProblemData{RPS: 12, AvgInputTokens: 16, AvgOutputTokens: 8, Accelerator: "H100", Model: "m"}
	body, _ := json.Marshal(pd)
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ad evaluator.AnalysisData
	if err := json.Unmarshal(rr.Body.Bytes(), &ad); err != nil {
		t.Fatal(err)
	}
	if ad.AvgTTFT == 0 {
		t.Errorf("expected non-zero AvgTTFT, got %+v", ad)
	}
}
