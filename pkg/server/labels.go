package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// Label keys written on the pod by the Load Emulator (load.*) and the
// Actuator (allocation.*). Must match control-loop pkg/controller/defaults.go.
const (
	labelRPM          = "inferno.server.load.rpm"
	labelInTokens     = "inferno.server.load.intokens"
	labelOutTokens    = "inferno.server.load.outtokens"
	labelModel        = "inferno.server.model"
	labelAccelerator  = "inferno.server.allocation.accelerator"
	labelMaxBatchSize = "inferno.server.allocation.maxbatchsize"
)

// ReadLabels parses a downward-API metadata.labels projection. Each line is
// key="value"; surrounding quotes are stripped. Missing file → error.
func ReadLabels(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read labels %s: %w", path, err)
	}
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out, nil
}

// LabelsToProblemData builds a ProblemData from pod labels. Returns ok=false
// when a required workload field is absent or non-positive — the pod is not
// yet ready to drive (e.g. the Load Emulator has not labelled it). RPS is
// derived from rpm. MaxConcurrency may be 0 (the evaluator resolves a default).
func LabelsToProblemData(labels map[string]string) (evaluator.ProblemData, bool) {
	rpm, err1 := strconv.ParseFloat(labels[labelRPM], 32)
	inTok, err2 := strconv.Atoi(labels[labelInTokens])
	outTok, err3 := strconv.Atoi(labels[labelOutTokens])
	model := labels[labelModel]
	acc := labels[labelAccelerator]
	if err1 != nil || err2 != nil || err3 != nil || rpm <= 0 || inTok <= 0 || outTok <= 0 || model == "" || acc == "" {
		return evaluator.ProblemData{}, false
	}
	maxBatch, _ := strconv.Atoi(labels[labelMaxBatchSize]) // 0 if absent → evaluator default
	return evaluator.ProblemData{
		RPS:             float32(rpm / 60.0),
		MaxConcurrency:  maxBatch,
		AvgInputTokens:  float32(inTok),
		AvgOutputTokens: float32(outTok),
		Accelerator:     acc,
		Model:           model,
	}, true
}
