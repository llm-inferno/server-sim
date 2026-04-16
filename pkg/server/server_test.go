package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
	"github.com/llm-inferno/server-sim/pkg/noise"
)

// mockEvaluator starts an httptest.Server that always returns the given AnalysisData.
func mockEvaluator(t *testing.T, resp evaluator.AnalysisData) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("mock evaluator encode: %v", err)
		}
	}))
}

// pollJob polls GET /simulate/:id until the job is no longer pending,
// then returns the response body.
func pollJob(t *testing.T, srv *httptest.Server, jobID string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/simulate/" + jobID)
		if err != nil {
			t.Fatalf("GET /simulate/%s: %v", jobID, err)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode poll response: %v", err)
		}
		resp.Body.Close()
		if body["status"] != "pending" {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s still pending after 5 seconds", jobID)
	return nil
}

// submitJob submits a POST /simulate request and returns the job ID.
func submitJob(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	pd := evaluator.ProblemData{
		RPS: 1.0, AvgInputTokens: 128, AvgOutputTokens: 64,
		Accelerator: "H100", Model: "test-model",
	}
	body, _ := json.Marshal(pd)
	resp, err := http.Post(srv.URL+"/simulate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /simulate: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	id, ok := out["jobID"].(string)
	if !ok || id == "" {
		t.Fatalf("no jobID in response: %v", out)
	}
	return id
}

// newTestServer creates a server-sim instance backed by the given mock evaluator.
func newTestServer(t *testing.T, evalURL string, noiseEnabled bool) *httptest.Server {
	t.Helper()
	cfg := config.Config{
		Port:         0,
		EvaluatorURL: evalURL,
		NoiseEnabled: noiseEnabled,
		Noise:        noise.Config{StdFraction: 1.0}, // large fraction: would change values if applied
		JobTTL:       time.Minute,
	}
	s := New(cfg)
	return httptest.NewServer(s.Handler())
}

// resultField extracts the "result" sub-map from a completed job response body.
func resultField(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	r, ok := body["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result field in body: %v", body)
	}
	return r
}

// ---------------------------------------------------------------------------

func TestServer_SaturatedResult_NoiseSkipped(t *testing.T) {
	// Evaluator returns a saturated result with specific metric values.
	evalResp := evaluator.AnalysisData{
		Throughput:  5.0,
		AvgRespTime: 200.0,
		MaxRPS:      2.0,
		Saturation:  evaluator.SaturationOverload,
	}
	eval := mockEvaluator(t, evalResp)
	defer eval.Close()

	// Run server-sim with noise ENABLED; saturation should suppress it.
	srv := newTestServer(t, eval.URL, true)
	defer srv.Close()

	jobID := submitJob(t, srv)
	body := pollJob(t, srv, jobID)

	if body["status"] != "completed" {
		t.Fatalf("expected status completed, got %v", body["status"])
	}
	result := resultField(t, body)

	// Saturation flag must be preserved.
	if result["saturation"] != evalResp.Saturation {
		t.Errorf("saturation = %v, want %q", result["saturation"], evalResp.Saturation)
	}

	// With StdFraction=1.0 noise, values would change substantially if applied.
	// Verify metrics are unchanged (noise was skipped).
	if got := result["throughput"].(float64); got != float64(evalResp.Throughput) {
		t.Errorf("throughput = %v, want %v (noise should not be applied to saturated result)",
			got, evalResp.Throughput)
	}
	if got := result["avgRespTime"].(float64); got != float64(evalResp.AvgRespTime) {
		t.Errorf("avgRespTime = %v, want %v (noise should not be applied to saturated result)",
			got, evalResp.AvgRespTime)
	}
}

func TestServer_UnsaturatedResult_NoiseApplied(t *testing.T) {
	// Evaluator returns a non-saturated result.
	evalResp := evaluator.AnalysisData{
		Throughput:  5.0,
		AvgRespTime: 200.0,
		MaxRPS:      10.0,
	}
	eval := mockEvaluator(t, evalResp)
	defer eval.Close()

	srv := newTestServer(t, eval.URL, true) // noise enabled
	defer srv.Close()

	// Run multiple jobs; at least one should differ from the original (noise active).
	anyChanged := false
	for i := 0; i < 20; i++ {
		jobID := submitJob(t, srv)
		body := pollJob(t, srv, jobID)
		if body["status"] != "completed" {
			t.Fatalf("expected completed, got %v", body["status"])
		}
		result := resultField(t, body)
		if result["throughput"].(float64) != float64(evalResp.Throughput) ||
			result["avgRespTime"].(float64) != float64(evalResp.AvgRespTime) {
			anyChanged = true
			break
		}
	}
	if !anyChanged {
		t.Error("noise is enabled for non-saturated result but no metric changed after 20 samples")
	}
}

func TestServer_SaturatedResult_SaturationFieldPresentInJSON(t *testing.T) {
	evalResp := evaluator.AnalysisData{
		Saturation: evaluator.SaturationBandwidth,
		MaxRPS:     3.0,
	}
	eval := mockEvaluator(t, evalResp)
	defer eval.Close()

	srv := newTestServer(t, eval.URL, false)
	defer srv.Close()

	jobID := submitJob(t, srv)
	body := pollJob(t, srv, jobID)
	result := resultField(t, body)

	if result["saturation"] != string(evaluator.SaturationBandwidth) {
		t.Errorf("saturation = %v, want %q", result["saturation"], evaluator.SaturationBandwidth)
	}
}

func TestServer_UnsaturatedResult_SaturationFieldAbsentFromJSON(t *testing.T) {
	evalResp := evaluator.AnalysisData{Throughput: 5.0, MaxRPS: 10.0}
	eval := mockEvaluator(t, evalResp)
	defer eval.Close()

	srv := newTestServer(t, eval.URL, false)
	defer srv.Close()

	jobID := submitJob(t, srv)
	body := pollJob(t, srv, jobID)
	result := resultField(t, body)

	if _, present := result["saturation"]; present {
		t.Errorf("saturation field should be absent (omitempty) when not saturated, got %v", result["saturation"])
	}
}
