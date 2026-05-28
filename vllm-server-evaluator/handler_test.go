package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// fullFakeVLLM serves /v1/models, /v1/completions (streaming), /metrics.
type fullFakeVLLM struct {
	servedModel string
	chunkCount  int
	chunkInter  time.Duration
	firstDelay  time.Duration
	queueSum    atomic.Pointer[float64]
	queueCount  atomic.Pointer[float64]
	inferSum    atomic.Pointer[float64]
	inferCount  atomic.Pointer[float64]
	completed   int64
}

func newFullFakeVLLM(t *testing.T, servedModel string, chunks int, inter, first time.Duration) (*httptest.Server, *fullFakeVLLM) {
	f := &fullFakeVLLM{servedModel: servedModel, chunkCount: chunks, chunkInter: inter, firstDelay: first}
	z := 0.0
	f.queueSum.Store(&z)
	f.queueCount.Store(&z)
	f.inferSum.Store(&z)
	f.inferCount.Store(&z)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": f.servedModel}}})
		case strings.HasSuffix(r.URL.Path, "/v1/completions"):
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			time.Sleep(f.firstDelay)
			for i := 0; i < f.chunkCount; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"text\":\"x\"}]}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				if i < f.chunkCount-1 {
					time.Sleep(f.chunkInter)
				}
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			atomic.AddInt64(&f.completed, 1)
		case r.URL.Path == "/metrics":
			fmt.Fprintf(w, "vllm:request_queue_time_seconds_sum %v\n", *f.queueSum.Load())
			fmt.Fprintf(w, "vllm:request_queue_time_seconds_count %v\n", *f.queueCount.Load())
			fmt.Fprintf(w, "vllm:request_inference_time_seconds_sum %v\n", *f.inferSum.Load())
			fmt.Fprintf(w, "vllm:request_inference_time_seconds_count %v\n", *f.inferCount.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestSolve_HappyPath(t *testing.T) {
	srv, _ := newFullFakeVLLM(t, "test-model", 5, 20*time.Millisecond, 50*time.Millisecond)

	r := gin.New()
	cfg := map[string]serverConfig{
		"H100|test-model": {
			VLLMServedModelName: "test-model",
			VLLMPort:            0,
			WarmupSec:           0,
			MinWindowSec:        1,
			MaxWindowSec:        3,
			TargetSamples:       10,
			MinSamples:          5,
			IgnoreEOS:           true,
			QueueTimeMetric:     "vllm:request_queue_time_seconds",
		},
	}
	state := &handlerState{
		Lookup: cfg,
		Pairing: &pairingState{
			VLLMPodIP: strings.TrimPrefix(srv.URL, "http://"),
			VLLMPort:  0,
		},
		BaseURLOverride: srv.URL, // test hook bypasses ip:port construction
	}
	r.POST("/solve", solveHandler(state))

	pd := evaluator.ProblemData{
		RPS:             20,
		AvgInputTokens:  16,
		AvgOutputTokens: 8,
		Accelerator:     "H100",
		Model:           "test-model",
	}
	body, _ := json.Marshal(pd)
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var ad evaluator.AnalysisData
	if err := json.Unmarshal(rr.Body.Bytes(), &ad); err != nil {
		t.Fatal(err)
	}
	if ad.AvgTTFT == 0 || ad.AvgITL == 0 || ad.Throughput == 0 {
		t.Errorf("expected non-zero metrics, got %+v", ad)
	}
}

func TestSolve_PairingNotReady(t *testing.T) {
	r := gin.New()
	state := &handlerState{
		Lookup: map[string]serverConfig{},
	}
	r.POST("/solve", solveHandler(state))

	body, _ := json.Marshal(evaluator.ProblemData{Accelerator: "H100", Model: "m"})
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestSolve_UnknownModel(t *testing.T) {
	r := gin.New()
	state := &handlerState{
		Lookup:  map[string]serverConfig{},
		Pairing: &pairingState{VLLMPodIP: "127.0.0.1"},
	}
	r.POST("/solve", solveHandler(state))

	body, _ := json.Marshal(evaluator.ProblemData{Accelerator: "H100", Model: "missing"})
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestSolve_ServedModelMismatch(t *testing.T) {
	srv, _ := newFullFakeVLLM(t, "DIFFERENT-MODEL", 1, time.Millisecond, time.Millisecond)

	r := gin.New()
	cfg := map[string]serverConfig{
		"H100|test-model": {
			VLLMServedModelName: "test-model",
			MinWindowSec:        1, MaxWindowSec: 2, TargetSamples: 5, MinSamples: 1,
			IgnoreEOS: true, QueueTimeMetric: "vllm:request_queue_time_seconds",
		},
	}
	state := &handlerState{
		Lookup:          cfg,
		Pairing:         &pairingState{VLLMPodIP: strings.TrimPrefix(srv.URL, "http://")},
		BaseURLOverride: srv.URL,
	}
	r.POST("/solve", solveHandler(state))

	body, _ := json.Marshal(evaluator.ProblemData{
		RPS: 10, AvgInputTokens: 4, AvgOutputTokens: 2,
		Accelerator: "H100", Model: "test-model",
	})
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (served-model mismatch), body=%s", rr.Code, rr.Body.String())
	}
}

// silence unused imports in some build configs
var _ = context.Background
