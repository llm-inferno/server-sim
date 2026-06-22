package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{
				"accelerator": "H100",
				"model": "ibm-granite/granite-3.1-8b-instruct",
				"vllmServedModelName": "granite",
				"vllmPort": 8000,
				"warmupSec": 5,
				"minWindowSec": 20,
				"maxWindowSec": 300,
				"targetSamples": 200,
				"minSamples": 50,
				"ignoreEOS": true,
				"queueTimeMetric": "vllm:request_queue_time_seconds"
			}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)

	lookup, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(lookup) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(lookup))
	}
	got, ok := lookup["H100|ibm-granite/granite-3.1-8b-instruct"]
	if !ok {
		t.Fatalf("missing expected key; have keys: %v", keysOf(lookup))
	}
	if got.VLLMServedModelName != "granite" {
		t.Errorf("VLLMServedModelName = %q, want %q", got.VLLMServedModelName, "granite")
	}
	if got.VLLMPort != 8000 {
		t.Errorf("VLLMPort = %d, want 8000", got.VLLMPort)
	}
	if got.WarmupSec != 5 {
		t.Errorf("WarmupSec = %d, want 5", got.WarmupSec)
	}
	if got.IgnoreEOS != true {
		t.Errorf("IgnoreEOS = %v, want true", got.IgnoreEOS)
	}
}

func TestLoadConfig_DefaultsServedModelName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{ "accelerator": "H100", "model": "m", "vllmPort": 8000,
			  "warmupSec": 1, "minWindowSec": 1, "maxWindowSec": 10,
			  "targetSamples": 10, "minSamples": 5,
			  "queueTimeMetric": "vllm:request_queue_time_seconds" }
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)

	lookup, _ := loadConfig()
	got := lookup["H100|m"]
	if got.VLLMServedModelName != "m" {
		t.Errorf("VLLMServedModelName fallback to model = %q, want %q", got.VLLMServedModelName, "m")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	t.Setenv("VLLM_EVAL_CONFIG_FILE", "/nonexistent/path.json")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestLoadConfig_TokenDistributions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{ "accelerator": "H100", "model": "m", "vllmPort": 8000,
			  "warmupSec": 1, "minWindowSec": 1, "maxWindowSec": 10,
			  "targetSamples": 10, "minSamples": 5,
			  "queueTimeMetric": "vllm:request_queue_time_seconds",
			  "inputTokenDistribution": "fixed",
			  "outputTokenDistribution": "geometric" }
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)

	lookup, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	got := lookup["H100|m"]
	if got.InputTokenDistribution != "fixed" {
		t.Errorf("InputTokenDistribution = %q, want %q", got.InputTokenDistribution, "fixed")
	}
	if got.OutputTokenDistribution != "geometric" {
		t.Errorf("OutputTokenDistribution = %q, want %q", got.OutputTokenDistribution, "geometric")
	}
}

func TestLoadConfig_DefaultsTokenDistributionsToEmpty(t *testing.T) {
	// Empty / missing fields are accepted and resolve to "fixed" in newSampler.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{ "accelerator": "H100", "model": "m", "vllmPort": 8000,
			  "warmupSec": 1, "minWindowSec": 1, "maxWindowSec": 10,
			  "targetSamples": 10, "minSamples": 5,
			  "queueTimeMetric": "vllm:request_queue_time_seconds" }
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)

	lookup, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	got := lookup["H100|m"]
	if got.InputTokenDistribution != "" || got.OutputTokenDistribution != "" {
		t.Errorf("defaults should be empty strings, got in=%q out=%q",
			got.InputTokenDistribution, got.OutputTokenDistribution)
	}
}

func TestLoadConfig_RejectsUnknownDistribution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{ "accelerator": "H100", "model": "m", "vllmPort": 8000,
			  "warmupSec": 1, "minWindowSec": 1, "maxWindowSec": 10,
			  "targetSamples": 10, "minSamples": 5,
			  "queueTimeMetric": "vllm:request_queue_time_seconds",
			  "outputTokenDistribution": "lognormal" }
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for unknown outputTokenDistribution, got nil")
	}
}

func TestLoadConfig_TrailingWindowDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"configs":[{"accelerator":"H100","model":"m","vllmServedModelName":"m","minSamples":5}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)
	lookup, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := lookup["H100|m"].TrailingWindowSec; got != 30 {
		t.Errorf("TrailingWindowSec = %d, want default 30", got)
	}
}

func keysOf(m map[string]serverConfig) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
