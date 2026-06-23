package evaluator

// Saturation reason constants returned in AnalysisData.Saturation.
// Empty string (SaturationNone) means the server is not saturated and metrics are reliable.
const (
	SaturationNone      = ""            // not saturated; all metrics are valid
	SaturationBandwidth = "bandwidth"   // decode memory bandwidth is the binding bottleneck
	SaturationKV        = "kv_capacity" // KV cache capacity is exhausted
	SaturationOverload  = "overloaded"  // generic overload (queue-analysis or post-sim DES indicators)
)

// ProblemData is the input to the evaluator /solve endpoint.
// It describes the workload and server identity. Evaluator-specific parameters
// (e.g. Alpha/Beta/Gamma for the analytical model) are derived by the evaluator
// from Accelerator and Model via its own configuration.
type ProblemData struct {
	RPS float32 `json:"RPS"` // offered load: arrival rate of requests to the server (requests/sec)
	// MaxConcurrency is the maximum concurrent requests in the server. 0 (omitted)
	// means "use the server's native/configured concurrency": each evaluator falls
	// back to its own configured default, and finally to DefaultMaxConcurrency.
	// See ResolveMaxConcurrency. (blis is stricter: it requires a positive
	// configured maxRunningReqs and never reaches the shared backstop.)
	MaxConcurrency  int     `json:"maxConcurrency"`
	AvgInputTokens  float32 `json:"avgInputTokens"`  // average input tokens per request
	AvgOutputTokens float32 `json:"avgOutputTokens"` // average output tokens per request
	Accelerator     string  `json:"accelerator"`     // accelerator type (e.g. "H100", "A100")
	Model           string  `json:"model"`           // LLM model name (e.g. "llama-3-8b")
}

// AnalysisData is the output from the evaluator /solve endpoint.
type AnalysisData struct {
	Throughput  float32 `json:"throughput"`  // goodput: departure rate of successfully completed requests (req/sec); Throughput ≤ RPS, with the difference representing dropped/rejected requests
	AvgRespTime float32 `json:"avgRespTime"` // average response time (ms)
	AvgWaitTime float32 `json:"avgWaitTime"` // average queueing time (ms)
	AvgTTFT     float32 `json:"avgTTFT"`     // average time-to-first-token (ms)
	AvgITL      float32 `json:"avgITL"`      // average inter-token latency (ms)
	MaxRPS      float32 `json:"maxRPS"`      // maximum stable request rate (req/sec)
	// OfferedRPS is the offered arrival rate (req/sec) averaged over the same window
	// as Throughput/latency, so the (offered, throughput, latency) triple is
	// temporally consistent. Set only by backends that measure a window-average
	// offered rate (continuous-vllm-server); 0/omitted means not measured — callers
	// then fall back to the commanded ProblemData.RPS.
	OfferedRPS float32 `json:"offeredRPS,omitempty"`
	// Saturation is set when the offered load exceeds server capacity. One of the
	// SaturationXxx constants; empty (omitted from JSON) means not saturated.
	// When set, latency metrics may be zero (BLIS pre-sim, DES skipped) or reflect
	// degraded-state behaviour (queue-analysis, post-sim). MaxRPS is populated where
	// computable. Noise is never applied to saturated results.
	Saturation string `json:"saturation,omitempty"`
}

// IsSaturated reports whether the server was detected as overloaded.
func (ad AnalysisData) IsSaturated() bool {
	return ad.Saturation != ""
}
