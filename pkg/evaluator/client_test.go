package evaluator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSolveCtxCancels(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // block until client cancels
		case <-done: // unblocked by test cleanup
		}
	}))

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	_, err := c.SolveCtx(ctx, ProblemData{RPS: 1})
	close(done) // unblock handler before Close
	srv.Close()
	if err == nil {
		t.Fatal("expected error on cancellation")
	}
}

func TestSolveCtxSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(AnalysisData{AvgITL: 7})
	}))
	defer srv.Close()
	ad, err := NewClient(srv.URL).SolveCtx(context.Background(), ProblemData{RPS: 1})
	if err != nil || ad.AvgITL != 7 {
		t.Fatalf("ad=%+v err=%v", ad, err)
	}
}
