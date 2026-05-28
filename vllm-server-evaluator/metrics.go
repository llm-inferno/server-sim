package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// scrapeMetrics fetches and parses vLLM's Prometheus /metrics endpoint,
// extracting queue-time and inference-time histogram aggregates. Other
// metrics in the response are ignored. Uses a 5s timeout.
func scrapeMetrics(ctx context.Context, url string) (metricsScrape, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return metricsScrape{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return metricsScrape{}, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metricsScrape{}, fmt.Errorf("scrape %s: status %d", url, resp.StatusCode)
	}

	var m metricsScrape
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// We only care about four exact metric names (no labels).
		switch {
		case strings.HasPrefix(line, "vllm:request_queue_time_seconds_sum "):
			m.QueueTimeSum = parseValue(line)
		case strings.HasPrefix(line, "vllm:request_queue_time_seconds_count "):
			m.QueueTimeCount = parseValue(line)
		case strings.HasPrefix(line, "vllm:request_inference_time_seconds_sum "):
			m.InferTimeSum = parseValue(line)
		case strings.HasPrefix(line, "vllm:request_inference_time_seconds_count "):
			m.InferTimeCount = parseValue(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return metricsScrape{}, fmt.Errorf("scan /metrics: %w", err)
	}
	return m, nil
}

// parseValue extracts the float value from a prom text line "metricname value".
// Returns 0 on parse failure (caller treats zero as "missing").
func parseValue(line string) float64 {
	idx := strings.LastIndex(line, " ")
	if idx < 0 {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64)
	if err != nil {
		return 0
	}
	return v
}

// windowDelta returns the per-completion mean of a histogram over a window:
// (sum_end - sum_start) / (count_end - count_start), in seconds. Returns 0
// if the count delta is non-positive.
func windowDelta(start, end float64, startCount, endCount float64) float64 {
	dc := endCount - startCount
	if dc <= 0 {
		return 0
	}
	return (end - start) / dc
}
