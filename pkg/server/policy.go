package server

import (
	"context"
	"fmt"

	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

const (
	overloadTargetUtilization = float32(0.95)
	overloadRetryStep         = float32(0.05)
	overloadMaxRetries        = 3
)

// solver is the subset of evaluator.Client used here (mockable in tests).
type solver interface {
	SolveCtx(context.Context, evaluator.ProblemData) (evaluator.AnalysisData, error)
}

// solveWithPolicy runs one window and applies the saturation policy. It returns
// the EFFECTIVE input actually run (post retry-adjustment) and the result.
func solveWithPolicy(ctx context.Context, cli solver, policy string, pd evaluator.ProblemData) (evaluator.ProblemData, evaluator.AnalysisData, error) {
	ad, err := cli.SolveCtx(ctx, pd)
	if err != nil {
		return pd, ad, err
	}
	if policy == config.SaturationPolicyPassThrough || !ad.IsSaturated() {
		return pd, ad, nil
	}
	// retry-at-lower-load
	util := overloadTargetUtilization
	eff := pd
	for attempt := 1; attempt <= overloadMaxRetries; attempt++ {
		eff = pd
		eff.RPS = ad.MaxRPS * util
		next, nerr := cli.SolveCtx(ctx, eff)
		if nerr != nil {
			return eff, ad, nerr
		}
		ad = next
		if !ad.IsSaturated() {
			return eff, ad, nil
		}
		util -= overloadRetryStep
	}
	return eff, ad, fmt.Errorf("still saturated after %d retries", overloadMaxRetries)
}
