package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const defaultSimulationHorizon = int64(60_000_000) // 60 seconds in microseconds
const defaultBlockSizeTokens = int64(16)
const defaultSeed = int64(42)

// modelEntry is one entry in blis-config.json, describing the BLIS simulation
// parameters for a specific accelerator+model pair.
type modelEntry struct {
	Accelerator        string    `json:"accelerator"`
	Model              string    `json:"model"`
	HFConfigPath       string    `json:"hfConfigPath"`       // path to HuggingFace config.json for the model
	HWConfigPath       string    `json:"hwConfigPath"`       // path to hardware_config.json (overrides HW_CONFIG_FILE env var when set)
	GPU                string    `json:"gpu"`                // GPU name matching hardware_config.json (e.g. "H100", "A100-SXM")
	TP                 int       `json:"tp"`                 // tensor parallelism degree
	TotalKVBlocks      int64     `json:"totalKVBlocks"`      // total GPU KV cache blocks
	BlockSizeTokens    int64     `json:"blockSizeTokens"`    // tokens per KV block (default 16)
	MaxRunningReqs     int64     `json:"maxRunningReqs"`     // max concurrent requests in running batch
	MaxScheduledTokens int64     `json:"maxScheduledTokens"` // max total new tokens across running batch
	MaxModelLen        int64     `json:"maxModelLen"`        // max sequence length (0 = unlimited)
	Scheduler          string    `json:"scheduler"`          // "fcfs" (default), "sjf", "priority-fcfs"
	AlphaCoeffs        []float64 `json:"alphaCoeffs"`        // queueing time regression coefficients [α₀, α₁, α₂] (µs)
	SimulationHorizon  int64     `json:"simulationHorizon"`  // sim duration in microseconds (default 60s)
	NumRequests        int64     `json:"numRequests"`        // max requests to simulate (0 = use horizon only)
	Seed               int64     `json:"seed"`               // RNG seed for deterministic results
}

// blisConfig is the top-level structure of blis-config.json.
type blisConfig struct {
	Models []modelEntry `json:"models"`
}

func modelKey(accelerator, model string) string {
	return accelerator + "|" + model
}

// loadConfig reads blis-config.json from the path given by BLIS_CONFIG_FILE
// (defaulting to "blis-config.json") and returns a lookup map keyed by
// "accelerator|model" for O(1) request-time access.
func loadConfig() (map[string]modelEntry, error) {
	path := os.Getenv("BLIS_CONFIG_FILE")
	if path == "" {
		path = "blis-config.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read blis config file %q: %w", path, err)
	}

	var bc blisConfig
	if err := json.Unmarshal(data, &bc); err != nil {
		return nil, fmt.Errorf("parse blis config file %q: %w", path, err)
	}

	lookup := make(map[string]modelEntry, len(bc.Models))
	for _, m := range bc.Models {
		if err := validateEntry(&m); err != nil {
			return nil, fmt.Errorf("invalid config entry for %s/%s: %w", m.Accelerator, m.Model, err)
		}
		applyDefaults(&m)
		lookup[modelKey(m.Accelerator, m.Model)] = m
	}
	return lookup, nil
}

// validateEntry checks required fields before defaults are applied.
func validateEntry(m *modelEntry) error {
	if m.Accelerator == "" {
		return fmt.Errorf("accelerator is required")
	}
	if m.Model == "" {
		return fmt.Errorf("model is required")
	}
	if m.HFConfigPath == "" {
		return fmt.Errorf("hfConfigPath is required")
	}
	if m.GPU == "" {
		return fmt.Errorf("gpu is required")
	}
	if m.TotalKVBlocks <= 0 {
		return fmt.Errorf("totalKVBlocks must be > 0")
	}
	if m.MaxRunningReqs <= 0 {
		return fmt.Errorf("maxRunningReqs must be > 0")
	}
	if m.MaxScheduledTokens <= 0 {
		return fmt.Errorf("maxScheduledTokens must be > 0")
	}
	return nil
}

// applyDefaults fills in zero-valued optional fields.
func applyDefaults(m *modelEntry) {
	if m.TP == 0 {
		m.TP = 1
	}
	if m.BlockSizeTokens == 0 {
		m.BlockSizeTokens = defaultBlockSizeTokens
	}
	if m.Scheduler == "" {
		m.Scheduler = "fcfs"
	}
	if m.SimulationHorizon == 0 {
		m.SimulationHorizon = defaultSimulationHorizon
	}
	if m.Seed == 0 {
		m.Seed = defaultSeed
	}
	if len(m.AlphaCoeffs) < 3 {
		// Default: zero queueing overhead (conservative — underestimates TTFT).
		// Supply calibrated values from defaults.yaml for accurate results.
		m.AlphaCoeffs = []float64{0, 0, 0}
	}
}
