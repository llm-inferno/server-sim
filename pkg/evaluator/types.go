package evaluator

// ProblemData is the input to the evaluator /solve endpoint.
// It describes the workload and server identity. Evaluator-specific parameters
// (e.g. Alpha/Beta/Gamma for the analytical model) are derived by the evaluator
// from Accelerator and Model via its own configuration.
type ProblemData struct {
	RPS             float32 `json:"RPS"`             // offered load: arrival rate of requests to the server (requests/sec)
	MaxConcurrency  int     `json:"maxConcurrency"`  // maximum concurrent requests in server
	AvgInputTokens  float32 `json:"avgInputTokens"`  // average input tokens per request
	AvgOutputTokens float32 `json:"avgOutputTokens"` // average output tokens per request
	Accelerator     string  `json:"accelerator"`     // accelerator type (e.g. "H100", "A100")
	Model           string  `json:"model"`           // LLM model name (e.g. "llama-3-8b")
}

// AnalysisData is the output from the evaluator /solve endpoint.
type AnalysisData struct {
	Throughput   float32 `json:"throughput"`   // goodput: departure rate of successfully completed requests (req/sec); Throughput ≤ RPS, with the difference representing dropped/rejected requests
	AvgRespTime  float32 `json:"avgRespTime"`  // average response time (ms)
	AvgWaitTime  float32 `json:"avgWaitTime"`  // average queueing time (ms)
	AvgTTFT      float32 `json:"avgTTFT"`      // average time-to-first-token (ms)
	AvgITL       float32 `json:"avgITL"`       // average inter-token latency (ms)
	MaxRPS       float32 `json:"maxRPS"`       // maximum stable request rate (req/sec)
}
