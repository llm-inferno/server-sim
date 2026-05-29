package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// completionsRequest mirrors vLLM's OpenAI-compatible /v1/completions body.
// Prompt is sent as []int (token IDs) — the standard OpenAI way to pass
// pre-tokenized input; vLLM v0.22+ dropped the non-standard prompt_token_ids field.
type completionsRequest struct {
	Model     string `json:"model"`
	Prompt    []int  `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	IgnoreEOS bool   `json:"ignore_eos"`
	Stream    bool   `json:"stream"`
}

// runOneRequest sends a single streaming /v1/completions request to the given
// vLLM base URL and returns the per-request sample. Caller is responsible for
// scheduling (Poisson) and for filtering out warmup samples.
//
// vllmBaseURL is e.g. "http://10.0.0.1:8000".
func runOneRequest(ctx context.Context, vllmBaseURL, model string, spec requestSpec, seed int64) sample {
	body := completionsRequest{
		Model:     model,
		Prompt:    syntheticPromptTokens(spec.InputTokens, seed),
		MaxTokens: spec.OutputTokens,
		IgnoreEOS: spec.IgnoreEOS,
		Stream:    true,
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vllmBaseURL+"/v1/completions", bytes.NewReader(buf))
	if err != nil {
		return sample{Failed: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sample{StartedAt: started, Failed: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{StartedAt: started, Failed: true, StatusCode: resp.StatusCode}
	}

	var firstChunkAt time.Time
	var prevChunkAt time.Time
	var itls []time.Duration

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			// Unexpected read error mid-stream → mark failed but report partial timing.
			return sample{StartedAt: started, TTFT: timeSince(started, firstChunkAt), ITLs: itls, ResponseTime: time.Since(started), Failed: true}
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		now := time.Now()
		if firstChunkAt.IsZero() {
			firstChunkAt = now
		} else {
			itls = append(itls, now.Sub(prevChunkAt))
		}
		prevChunkAt = now
	}

	return sample{
		StartedAt:    started,
		TTFT:         timeSince(started, firstChunkAt),
		ITLs:         itls,
		ResponseTime: time.Since(started),
		StatusCode:   resp.StatusCode,
	}
}

// timeSince returns t.Sub(start), or 0 if t is zero.
func timeSince(start, t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return t.Sub(start)
}

// windowParams configures one measurement window.
type windowParams struct {
	BaseURL       string
	Model         string
	Spec          requestSpec
	RPS           float64
	WarmupSec     int
	MinWindowSec  int
	MaxWindowSec  int
	TargetSamples int
	Concurrency   int
	Seed          int64
}

// runWindow drives a Poisson stream of requests at wp.RPS for
// max(MinWindowSec, TargetSamples/RPS) seconds, capped at MaxWindowSec.
// Samples started during the warmup prefix are discarded from results.
//
// Concurrency limits the number of simultaneous in-flight requests; arrivals
// that would exceed it are simply dropped from this driver (mimicking real
// load that vLLM would queue itself — the per-request sample includes vLLM's
// own queue time).
func runWindow(ctx context.Context, wp windowParams) (*windowResult, error) {
	if wp.RPS <= 0 {
		return nil, fmt.Errorf("non-positive RPS: %v", wp.RPS)
	}
	if wp.Concurrency <= 0 {
		wp.Concurrency = 64
	}
	if wp.Seed == 0 {
		wp.Seed = time.Now().UnixNano()
	}

	rng := rand.New(rand.NewSource(wp.Seed))

	// Compute window length.
	target := float64(wp.TargetSamples) / wp.RPS
	wantSec := math.Max(float64(wp.MinWindowSec), target)
	if wantSec > float64(wp.MaxWindowSec) {
		wantSec = float64(wp.MaxWindowSec)
	}
	totalSec := float64(wp.WarmupSec) + wantSec
	deadline := time.Now().Add(time.Duration(totalSec * float64(time.Second)))
	windowStart := time.Now().Add(time.Duration(wp.WarmupSec) * time.Second)

	sem := make(chan struct{}, wp.Concurrency)
	var mu sync.Mutex
	var samples []sample
	var warmup int
	var wg sync.WaitGroup

	deadlineCh := time.After(time.Until(deadline))

	// Poisson interarrival: exponential with mean 1/RPS.
	for {
		gap := time.Duration(rng.ExpFloat64() / wp.RPS * float64(time.Second))
		select {
		case <-time.After(gap):
		case <-deadlineCh:
			// Deadline fired while waiting for the next Poisson arrival; stop immediately.
			windowEnd := time.Now()
			wg.Wait()
			return &windowResult{Samples: samples, WindowStart: windowStart, WindowEnd: windowEnd, WarmupSamples: warmup}, nil
		case <-ctx.Done():
			windowEnd := time.Now()
			wg.Wait()
			return &windowResult{Samples: samples, WindowStart: windowStart, WindowEnd: windowEnd, WarmupSamples: warmup}, ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}

		// Try to acquire concurrency slot; drop if full.
		select {
		case sem <- struct{}{}:
		default:
			continue
		}

		startedAt := time.Now()
		isWarmup := startedAt.Before(windowStart)
		seed := rng.Int63()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s := runOneRequest(ctx, wp.BaseURL, wp.Model, wp.Spec, seed)
			s.StartedAt = startedAt
			mu.Lock()
			defer mu.Unlock()
			if isWarmup {
				warmup++
				return
			}
			samples = append(samples, s)
		}()
	}
	windowEnd := time.Now()
	wg.Wait()

	return &windowResult{
		Samples:       samples,
		WindowStart:   windowStart,
		WindowEnd:     windowEnd,
		WarmupSamples: warmup,
	}, nil
}
