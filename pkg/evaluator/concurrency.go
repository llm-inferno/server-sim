package evaluator

import "log"

// DefaultMaxConcurrency is the last-resort concurrency used when neither the
// request (ProblemData.MaxConcurrency) nor the evaluator's own configuration
// specifies a value. 256 matches vLLM's historical / A100-class max_num_seqs
// default; it is a conservative backstop, not a claim to match every pod
// (current vLLM uses 1024 on H100/H200-class hardware).
const DefaultMaxConcurrency = 256

// ResolveMaxConcurrency picks the effective max concurrency for an evaluator,
// applying the uniform precedence: the request value if positive, else the
// evaluator's configured default if positive, else DefaultMaxConcurrency.
//
// Reaching the backstop means neither the request nor the configuration
// supplied a value — almost always a misconfiguration — so it is logged loudly
// rather than applied silently. who identifies the calling evaluator in that
// warning (e.g. "queue-analysis", "vllm-server", "dummy").
//
// blis intentionally does not use this helper: it validates its configured
// concurrency (maxRunningReqs > 0) at load time and fails loud on misconfig.
func ResolveMaxConcurrency(requested, configured int, who string) int {
	if requested > 0 {
		return requested
	}
	if configured > 0 {
		return configured
	}
	log.Printf("%s: maxConcurrency unspecified and no configured default; falling back to %d", who, DefaultMaxConcurrency)
	return DefaultMaxConcurrency
}
