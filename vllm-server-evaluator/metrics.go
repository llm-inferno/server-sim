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
// extracting the configured queue-time histogram and the inference-time
// histogram. queueTimeMetric is the metric base name (e.g.
// "vllm:request_queue_time_seconds"); the function reads its _sum and _count
// series. Other metrics are ignored. Uses a 5s timeout.
func scrapeMetrics(ctx context.Context, url, queueTimeMetric string) (metricsScrape, error) {
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

	queueSumPrefix := queueTimeMetric + "_sum "
	queueCountPrefix := queueTimeMetric + "_count "
	const inferSumPrefix = "vllm:request_inference_time_seconds_sum "
	const inferCountPrefix = "vllm:request_inference_time_seconds_count "

	var m metricsScrape
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, queueSumPrefix):
			m.QueueTimeSum = parseValue(line)
		case strings.HasPrefix(line, queueCountPrefix):
			m.QueueTimeCount = parseValue(line)
		case strings.HasPrefix(line, inferSumPrefix):
			m.InferTimeSum = parseValue(line)
		case strings.HasPrefix(line, inferCountPrefix):
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
