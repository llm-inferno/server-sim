package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeVLLMServer returns an httptest.Server that emits N SSE chunks at the
// given inter-chunk interval, then sends [DONE].
func fakeVLLMServer(t *testing.T, numChunks int, interval time.Duration, firstChunkDelay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/completions") {
			http.NotFound(w, r)
			return
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(firstChunkDelay)
		for i := 0; i < numChunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"text\":\"x\"}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			if i < numChunks-1 {
				time.Sleep(interval)
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func TestRunOneRequest_TTFTandITL(t *testing.T) {
	srv := fakeVLLMServer(t, 4, 50*time.Millisecond, 100*time.Millisecond)
	defer srv.Close()

	spec := requestSpec{InputTokens: 8, OutputTokens: 4, IgnoreEOS: true}
	s := runOneRequest(context.Background(), srv.URL, "test-model", spec, 1)

	if s.Failed {
		t.Fatalf("request failed: status=%d", s.StatusCode)
	}
	if s.TTFT < 90*time.Millisecond || s.TTFT > 200*time.Millisecond {
		t.Errorf("TTFT = %v, want ~100ms", s.TTFT)
	}
	if len(s.ITLs) != 3 { // 4 chunks → 3 inter-arrival deltas
		t.Errorf("ITLs len = %d, want 3", len(s.ITLs))
	}
	for _, itl := range s.ITLs {
		if itl < 30*time.Millisecond || itl > 100*time.Millisecond {
			t.Errorf("ITL = %v, want ~50ms", itl)
		}
	}
}

func TestRunOneRequest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	spec := requestSpec{InputTokens: 4, OutputTokens: 2, IgnoreEOS: true}
	s := runOneRequest(context.Background(), srv.URL, "test-model", spec, 1)
	if !s.Failed {
		t.Errorf("expected Failed=true on 500")
	}
	if s.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", s.StatusCode)
	}
}

func TestRunWindow_CollectsExpectedSampleCount(t *testing.T) {
	srv := fakeVLLMServer(t, 2, 10*time.Millisecond, 20*time.Millisecond)
	defer srv.Close()

	wp := windowParams{
		BaseURL:       srv.URL,
		Model:         "m",
		Spec:          requestSpec{InputTokens: 4, OutputTokens: 2, IgnoreEOS: true},
		RPS:           20.0,
		WarmupSec:     0,
		MinWindowSec:  1,
		MaxWindowSec:  3,
		TargetSamples: 10,
		Concurrency:   8,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := runWindow(ctx, wp)
	if err != nil {
		t.Fatalf("runWindow: %v", err)
	}
	if len(res.Samples) < 10 {
		t.Errorf("samples = %d, want >= 10 (target reached)", len(res.Samples))
	}
}

func TestRunWindow_CapsAtMaxWindow(t *testing.T) {
	srv := fakeVLLMServer(t, 2, 10*time.Millisecond, 20*time.Millisecond)
	defer srv.Close()

	wp := windowParams{
		BaseURL:       srv.URL,
		Model:         "m",
		Spec:          requestSpec{InputTokens: 4, OutputTokens: 2, IgnoreEOS: true},
		RPS:           1.0, // very slow
		WarmupSec:     0,
		MinWindowSec:  1,
		MaxWindowSec:  2,    // 1.0 RPS over 2s = ~2 samples max — far below target
		TargetSamples: 1000,
		Concurrency:   2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := runWindow(ctx, wp)
	if err != nil {
		t.Fatalf("runWindow: %v", err)
	}
	elapsed := res.WindowEnd.Sub(res.WindowStart)
	if elapsed < 1*time.Second || elapsed > 3*time.Second {
		t.Errorf("window length = %v, want ~2s (capped)", elapsed)
	}
}
