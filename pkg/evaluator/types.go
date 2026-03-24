package evaluator

// ProblemData is the input to the evaluator /solve endpoint.
// Schema matches queue-analysis ProblemData exactly.
type ProblemData struct {
	RPS             float32 `json:"RPS"`             // request arrival rate (requests/sec)
	MaxBatchSize    int     `json:"maxBatchSize"`    // maximum batch size
	AvgInputTokens  float32 `json:"avgInputTokens"`  // average input tokens per request
	AvgOutputTokens float32 `json:"avgOutputTokens"` // average output tokens per request
	Alpha           float32 `json:"alpha"`           // base iteration time (ms)
	Beta            float32 `json:"beta"`            // per-token prefill cost (ms/token)
	Gamma           float32 `json:"gamma"`           // quadratic batch/token interaction (ms/token²)
	MaxQueueSize    int     `json:"maxQueueSize"`    // maximum queue size
}

// AnalysisData is the output from the evaluator /solve endpoint.
type AnalysisData struct {
	Throughput   float32 `json:"throughput"`   // effective throughput (req/sec)
	AvgRespTime  float32 `json:"avgRespTime"`  // average response time (ms)
	AvgWaitTime  float32 `json:"avgWaitTime"`  // average queueing time (ms)
	AvgNumInServ float32 `json:"avgNumInServ"` // average number of requests in system
	AvgTTFT      float32 `json:"avgTTFT"`      // average time-to-first-token (ms)
	AvgITL       float32 `json:"avgITL"`       // average inter-token latency (ms)
	MaxRPS       float32 `json:"maxRPS"`       // maximum stable request rate (req/sec)
}
