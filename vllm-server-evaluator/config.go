package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// configEntry is one entry in vllm-eval-config.json.
type configEntry struct {
	Accelerator             string `json:"accelerator"`
	Model                   string `json:"model"`
	VLLMServedModelName     string `json:"vllmServedModelName"`
	VLLMPort                int    `json:"vllmPort"`
	WarmupSec               int    `json:"warmupSec"`
	MinWindowSec            int    `json:"minWindowSec"`
	MaxWindowSec            int    `json:"maxWindowSec"`
	TargetSamples           int    `json:"targetSamples"`
	MinSamples              int    `json:"minSamples"`
	IgnoreEOS               bool   `json:"ignoreEOS"`
	QueueTimeMetric         string `json:"queueTimeMetric"`
	InputTokenDistribution  string `json:"inputTokenDistribution"`
	OutputTokenDistribution string `json:"outputTokenDistribution"`
	DefaultConcurrency      int    `json:"defaultConcurrency"` // client-side in-flight cap when request omits maxConcurrency; 0 → evaluator.DefaultMaxConcurrency
}

// configFile is the top-level structure of vllm-eval-config.json.
type configFile struct {
	Configs []configEntry `json:"configs"`
}

// serverConfig is the validated, lookup-ready measurement policy for one
// (accelerator, model) pair.
type serverConfig struct {
	VLLMServedModelName     string
	VLLMPort                int
	WarmupSec               int
	MinWindowSec            int
	MaxWindowSec            int
	TargetSamples           int
	MinSamples              int
	IgnoreEOS               bool
	QueueTimeMetric         string
	InputTokenDistribution  string // "" defaults to "fixed"
	OutputTokenDistribution string // "" defaults to "fixed"
	DefaultConcurrency      int    // 0 → evaluator.DefaultMaxConcurrency at resolution time
}

// loadConfig reads vllm-eval-config.json from VLLM_EVAL_CONFIG_FILE
// (default: vllm-eval-config.json) and returns a lookup map keyed
// by "accelerator|model".
//
// VLLMServedModelName defaults to the model name when empty.
func loadConfig() (map[string]serverConfig, error) {
	path := os.Getenv("VLLM_EVAL_CONFIG_FILE")
	if path == "" {
		path = "vllm-eval-config.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vllm eval config %q: %w", path, err)
	}

	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse vllm eval config %q: %w", path, err)
	}

	lookup := make(map[string]serverConfig, len(cf.Configs))
	for _, e := range cf.Configs {
		served := e.VLLMServedModelName
		if served == "" {
			served = e.Model
		}
		// Validate distribution strings up front so misconfiguration fails at
		// startup rather than mid-run; sample with avg=2 (any avg ≥ 2 works).
		if _, err := newSampler(e.InputTokenDistribution, 2); err != nil {
			return nil, fmt.Errorf("config %s/%s: inputTokenDistribution: %w", e.Accelerator, e.Model, err)
		}
		if _, err := newSampler(e.OutputTokenDistribution, 2); err != nil {
			return nil, fmt.Errorf("config %s/%s: outputTokenDistribution: %w", e.Accelerator, e.Model, err)
		}
		// 0 is valid (falls back to the shared backstop at resolution time);
		// negative is a misconfiguration and must fail loud at startup.
		if e.DefaultConcurrency < 0 {
			return nil, fmt.Errorf("config %s/%s: defaultConcurrency must be >= 0", e.Accelerator, e.Model)
		}
		lookup[e.Accelerator+"|"+e.Model] = serverConfig{
			VLLMServedModelName:     served,
			VLLMPort:                e.VLLMPort,
			WarmupSec:               e.WarmupSec,
			MinWindowSec:            e.MinWindowSec,
			MaxWindowSec:            e.MaxWindowSec,
			TargetSamples:           e.TargetSamples,
			MinSamples:              e.MinSamples,
			IgnoreEOS:               e.IgnoreEOS,
			QueueTimeMetric:         e.QueueTimeMetric,
			InputTokenDistribution:  e.InputTokenDistribution,
			OutputTokenDistribution: e.OutputTokenDistribution,
			DefaultConcurrency:      e.DefaultConcurrency,
		}
	}
	return lookup, nil
}
