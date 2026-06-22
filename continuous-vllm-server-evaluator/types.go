package main

import "time"

// requestSpec is the synthetic request the generator sends to vLLM.
type requestSpec struct {
	InputTokens  int
	OutputTokens int
	IgnoreEOS    bool
}

// sample is one completed measurement from a single request.
type sample struct {
	StartedAt    time.Time
	TTFT         time.Duration
	ITLs         []time.Duration // per-chunk inter-arrival deltas after first chunk
	ResponseTime time.Duration
	Failed       bool   // network or server error
	StatusCode   int    // 0 if no response received
}

// metricsScrape is the minimal slice of vLLM /metrics we read.
type metricsScrape struct {
	QueueTimeSum   float64 // vllm:request_queue_time_seconds_sum
	QueueTimeCount float64 // vllm:request_queue_time_seconds_count
	InferTimeSum   float64 // vllm:request_inference_time_seconds_sum
	InferTimeCount float64 // vllm:request_inference_time_seconds_count
}
