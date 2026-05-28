package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScrapeMetrics_Parses(t *testing.T) {
	body := `# HELP vllm:request_queue_time_seconds Request queue time
# TYPE vllm:request_queue_time_seconds histogram
vllm:request_queue_time_seconds_bucket{le="0.1"} 5
vllm:request_queue_time_seconds_bucket{le="+Inf"} 10
vllm:request_queue_time_seconds_sum 1.25
vllm:request_queue_time_seconds_count 10
# HELP vllm:request_inference_time_seconds Inference time
# TYPE vllm:request_inference_time_seconds histogram
vllm:request_inference_time_seconds_sum 5.0
vllm:request_inference_time_seconds_count 10
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := scrapeMetrics(context.Background(), srv.URL+"/metrics")
	if err != nil {
		t.Fatalf("scrapeMetrics: %v", err)
	}
	if got.QueueTimeSum != 1.25 {
		t.Errorf("QueueTimeSum = %v, want 1.25", got.QueueTimeSum)
	}
	if got.QueueTimeCount != 10 {
		t.Errorf("QueueTimeCount = %v, want 10", got.QueueTimeCount)
	}
	if got.InferTimeSum != 5.0 {
		t.Errorf("InferTimeSum = %v, want 5.0", got.InferTimeSum)
	}
	if got.InferTimeCount != 10 {
		t.Errorf("InferTimeCount = %v, want 10", got.InferTimeCount)
	}
}

func TestScrapeMetrics_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := scrapeMetrics(context.Background(), srv.URL+"/metrics"); err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}
