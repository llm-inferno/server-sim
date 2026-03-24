package evaluator

import (
	"bytes"
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
	body, err := json.Marshal(pd)
	if err != nil {
		return AnalysisData{}, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := c.httpClient.Post(c.baseURL+"/solve", "application/json", bytes.NewReader(body))
	if err != nil {
		return AnalysisData{}, fmt.Errorf("POST /solve: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return AnalysisData{}, fmt.Errorf("evaluator returned status %d", resp.StatusCode)
	}

	var ad AnalysisData
	if err := json.NewDecoder(resp.Body).Decode(&ad); err != nil {
		return AnalysisData{}, fmt.Errorf("decode response: %w", err)
	}
	return ad, nil
}
