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
