package server

import (
	"context"
	"testing"

	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

type scriptedSolver struct {
	calls   int
	results []evaluator.AnalysisData
	lastRPS []float32
}

func (s *scriptedSolver) SolveCtx(_ context.Context, pd evaluator.ProblemData) (evaluator.AnalysisData, error) {
	s.lastRPS = append(s.lastRPS, pd.RPS)
	r := s.results[s.calls]
	s.calls++
	return r, nil
}

func TestPolicyPassThroughKeepsSaturated(t *testing.T) {
	s := &scriptedSolver{results: []evaluator.AnalysisData{{Saturation: evaluator.SaturationKV, MaxRPS: 4}}}
	pd := evaluator.ProblemData{RPS: 10}
	eff, ad, err := solveWithPolicy(context.Background(), s, config.SaturationPolicyPassThrough, pd)
	if err != nil {
		t.Fatalf("pass-through should not error: %v", err)
	}
	if ad.Saturation == "" || eff.RPS != 10 || s.calls != 1 {
		t.Fatalf("pass-through wrong: calls=%d eff=%v ad=%v", s.calls, eff, ad)
	}
}

func TestPolicyRetryRecoversUnsaturated(t *testing.T) {
	s := &scriptedSolver{results: []evaluator.AnalysisData{
		{Saturation: evaluator.SaturationOverload, MaxRPS: 4}, // first: saturated
		{Saturation: "", MaxRPS: 4},                           // retry at 0.95*4=3.8: ok
	}}
	pd := evaluator.ProblemData{RPS: 10}
	eff, ad, err := solveWithPolicy(context.Background(), s, config.SaturationPolicyRetry, pd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ad.Saturation != "" {
		t.Fatalf("should have recovered unsaturated")
	}
	if eff.RPS != float32(4*0.95) {
		t.Fatalf("effective RPS = %v, want %v", eff.RPS, float32(4*0.95))
	}
}

func TestPolicyRetrySaturatedWithoutMaxRPSErrors(t *testing.T) {
	// Saturated but MaxRPS not computable (0). Must fail the window rather than
	// retry at RPS = 0*util = 0, which would publish a bogus zero-load result.
	s := &scriptedSolver{results: []evaluator.AnalysisData{{Saturation: evaluator.SaturationKV, MaxRPS: 0}}}
	pd := evaluator.ProblemData{RPS: 10}
	_, _, err := solveWithPolicy(context.Background(), s, config.SaturationPolicyRetry, pd)
	if err == nil {
		t.Fatal("expected error when saturated with MaxRPS=0")
	}
	if s.calls != 1 {
		t.Fatalf("should not retry when MaxRPS<=0: calls=%d", s.calls)
	}
}

func TestPolicyRetryExhaustedErrors(t *testing.T) {
	s := &scriptedSolver{results: []evaluator.AnalysisData{
		{Saturation: "x", MaxRPS: 4}, {Saturation: "x", MaxRPS: 4},
		{Saturation: "x", MaxRPS: 4}, {Saturation: "x", MaxRPS: 4},
	}}
	eff, ad, err := solveWithPolicy(context.Background(), s, config.SaturationPolicyRetry, evaluator.ProblemData{RPS: 10})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// The last retry runs at util = 0.95 - 2*0.05 = 0.85, so eff.RPS = MaxRPS*0.85.
	if d := eff.RPS - float32(4*0.85); d < -1e-4 || d > 1e-4 {
		t.Fatalf("effective RPS = %v, want ~%v (MaxRPS*0.85)", eff.RPS, float32(4*0.85))
	}
	if !ad.IsSaturated() {
		t.Fatalf("result should remain saturated on exhaustion, got %+v", ad)
	}
}
