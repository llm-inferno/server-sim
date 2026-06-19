package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultTimeout = 10 * time.Minute

// Client sends workload to an evaluator backend and retrieves performance metrics.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new evaluator Client targeting the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// Solve calls POST {baseURL}/solve with the given ProblemData and returns AnalysisData.
func (c *Client) Solve(pd ProblemData) (AnalysisData, error) {
	return c.SolveCtx(context.Background(), pd)
}

// SolveCtx is Solve with caller-controlled cancellation. Cancelling ctx aborts
// the in-flight request, which stops the evaluator's measurement window.
func (c *Client) SolveCtx(ctx context.Context, pd ProblemData) (AnalysisData, error) {
	body, err := json.Marshal(pd)
	if err != nil {
		return AnalysisData{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/solve", bytes.NewReader(body))
	if err != nil {
		return AnalysisData{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return AnalysisData{}, fmt.Errorf("POST /solve: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return AnalysisData{}, fmt.Errorf("evaluator returned status %d", resp.StatusCode)
	}
	var ad AnalysisData
	if err := json.NewDecoder(resp.Body).Decode(&ad); err != nil {
		return AnalysisData{}, fmt.Errorf("decode response: %w", err)
	}
	return ad, nil
}
