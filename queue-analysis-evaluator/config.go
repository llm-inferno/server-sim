package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

const defaultMaxQueueSize = 0

// perfParms holds the queue-analysis timing coefficients for a server.
type perfParms struct {
	Alpha float32 `json:"alpha"`
	Beta  float32 `json:"beta"`
	Gamma float32 `json:"gamma"`
}

// modelEntry is one entry in model-data.json.
type modelEntry struct {
	Name         string    `json:"name"`
	Acc          string    `json:"acc"`
	MaxBatchSize int       `json:"maxBatchSize"`
	PerfParms    perfParms `json:"perfParms"`
}

// modelData is the top-level structure of model-data.json.
type modelData struct {
	Models []modelEntry `json:"models"`
}

// serverConfig holds all parameters needed by the queue-analysis evaluator
// for a specific accelerator+model pair.
type serverConfig struct {
	Alpha        float32
	Beta         float32
	Gamma        float32
	MaxBatchSize int
	MaxQueueSize int
}

// loadConfig reads model-data.json from the path given by MODEL_DATA_FILE
// (defaulting to "model-data.json") and returns a lookup map keyed by
// "acc|name" for O(1) request-time access.
//
// MaxQueueSize is not in model-data.json; it is read from DEFAULT_MAX_QUEUE_SIZE
// (defaulting to 0, i.e. no external queue) and applied uniformly to all entries.
func loadConfig() (map[string]serverConfig, error) {
	path := os.Getenv("MODEL_DATA_FILE")
	if path == "" {
		path = "model-data.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model data file %q: %w", path, err)
	}

	var md modelData
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("parse model data file %q: %w", path, err)
	}

	maxQueueSize := defaultMaxQueueSize
	if v := os.Getenv("DEFAULT_MAX_QUEUE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxQueueSize = n
		}
	}

	lookup := make(map[string]serverConfig, len(md.Models))
	for _, m := range md.Models {
		key := m.Acc + "|" + m.Name
		lookup[key] = serverConfig{
			Alpha:        m.PerfParms.Alpha,
			Beta:         m.PerfParms.Beta,
			Gamma:        m.PerfParms.Gamma,
			MaxBatchSize: m.MaxBatchSize,
			MaxQueueSize: maxQueueSize,
		}
	}
	return lookup, nil
}
