package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
