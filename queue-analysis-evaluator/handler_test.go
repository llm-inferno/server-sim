package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// testLookup returns a lookup map with one entry using plausible Alpha/Beta/Gamma
// values.  These are taken from the queue-analysis-evaluator sample model-data
// for a small model on an A100, adjusted to give a MaxRate around 2–4 req/s for
// 128/64 token requests so the saturation boundary is easy to straddle in tests.
func testLookup() map[string]serverConfig {
	return map[string]serverConfig{
		"H100|test-model": {
			Alpha:        50.0,
			Beta:         0.001,
			Gamma:        0.002,
			MaxBatchSize: 32,
			MaxQueueSize: 128,
		},
	}
}

// newTestRouter wires solveHandler with the test lookup into a Gin engine.
func newTestRouter() *gin.Engine {
	r := gin.New()
	r.POST("/solve", solveHandler(testLookup()))
	return r
}

// solve sends a POST /solve request and returns the decoded AnalysisData.
func solve(t *testing.T, r *gin.Engine, pd evaluator.ProblemData) (evaluator.AnalysisData, int) {
	t.Helper()
	body, err := json.Marshal(pd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var ad evaluator.AnalysisData
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&ad); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return ad, w.Code
}

func TestQueueAnalysisHandler_UnknownModel_Returns400(t *testing.T) {
	r := newTestRouter()
	pd := evaluator.ProblemData{
		RPS: 1.0, AvgInputTokens: 128, AvgOutputTokens: 64,
		Accelerator: "A100", Model: "nonexistent",
	}
	_, code := solve(t, r, pd)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown model", code)
	}
}

func TestQueueAnalysisHandler_SubCapacityLoad_NotSaturated(t *testing.T) {
	r := newTestRouter()
	// Very low RPS — well below MaxRate for any reasonable config.
	pd := evaluator.ProblemData{
		RPS: 0.01, AvgInputTokens: 128, AvgOutputTokens: 64,
		Accelerator: "H100", Model: "test-model",
	}
	ad, code := solve(t, r, pd)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ad.IsSaturated() {
		t.Errorf("saturation = %q, want none for very low RPS", ad.Saturation)
	}
	if ad.MaxRPS <= 0 {
		t.Errorf("maxRPS = %v, want > 0", ad.MaxRPS)
	}
}

func TestQueueAnalysisHandler_OverCapacityLoad_SaturationFlagged(t *testing.T) {
	r := newTestRouter()

	// First, find the MaxRate by running at very low RPS.
	pdProbe := evaluator.ProblemData{
		RPS: 0.001, AvgInputTokens: 128, AvgOutputTokens: 64,
		Accelerator: "H100", Model: "test-model",
	}
	probe, code := solve(t, r, pdProbe)
	if code != http.StatusOK {
		t.Fatalf("probe request failed with status %d", code)
	}
	if probe.MaxRPS <= 0 {
		t.Fatalf("probe returned non-positive MaxRPS %v", probe.MaxRPS)
	}

	// Now request well above MaxRate so the 2% margin is safely exceeded.
	pdHigh := pdProbe
	pdHigh.RPS = probe.MaxRPS * 2.0
	ad, code := solve(t, r, pdHigh)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (saturation should not fail the request)", code)
	}
	if !ad.IsSaturated() {
		t.Errorf("saturation = %q, want overloaded for RPS (%.2f) >> MaxRate (%.2f)",
			ad.Saturation, pdHigh.RPS, probe.MaxRPS)
	}
	if ad.Saturation != evaluator.SaturationOverload {
		t.Errorf("saturation = %q, want %q", ad.Saturation, evaluator.SaturationOverload)
	}
	// MaxRPS should still be populated.
	if ad.MaxRPS <= 0 {
		t.Errorf("maxRPS = %v, want > 0 even when saturated", ad.MaxRPS)
	}
}

func TestQueueAnalysisHandler_OverCapacityLoad_MetricsStillPresent(t *testing.T) {
	r := newTestRouter()

	// Probe for MaxRate.
	pdProbe := evaluator.ProblemData{
		RPS: 0.001, AvgInputTokens: 128, AvgOutputTokens: 64,
		Accelerator: "H100", Model: "test-model",
	}
	probe, _ := solve(t, r, pdProbe)

	pdHigh := pdProbe
	pdHigh.RPS = probe.MaxRPS * 2.0
	ad, _ := solve(t, r, pdHigh)

	// Metrics should be populated (degraded-state values), not zeroed.
	if ad.Throughput == 0 && ad.AvgRespTime == 0 && ad.AvgTTFT == 0 {
		t.Error("all metrics are zero on a saturated response — expected degraded-state values to be present")
	}
}
