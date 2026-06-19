package config

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/llm-inferno/server-sim/pkg/noise"
)

const (
	SaturationPolicyRetry       = "retry-at-lower-load"
	SaturationPolicyPassThrough = "pass-through"
)

// Config holds server-sim runtime configuration.
type Config struct {
	Port             int
	EvaluatorURL     string
	NoiseEnabled     bool
	Noise            noise.Config
	JobTTL           time.Duration
	ContinuousMode   bool
	TickInterval     time.Duration
	SaturationPolicy string
	LabelsDir        string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	cfg := Config{
		Port:             8080,
		EvaluatorURL:     "http://localhost:8081",
		NoiseEnabled:     false,
		Noise:            noise.Config{StdFraction: 0.05},
		JobTTL:           60 * time.Minute,
		ContinuousMode:   false,
		TickInterval:     5 * time.Second,
		SaturationPolicy: SaturationPolicyRetry,
		LabelsDir:        "/etc/podinfo",
	}

	if v := os.Getenv("SERVERSIM_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if v := os.Getenv("EVALUATOR_URL"); v != "" {
		cfg.EvaluatorURL = v
	}
	if v := os.Getenv("NOISE_ENABLED"); v == "true" {
		cfg.NoiseEnabled = true
	}
	if v := os.Getenv("NOISE_STD_FRACTION"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Noise.StdFraction = f
		}
	}
	if v := os.Getenv("JOB_TTL_MINUTES"); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m > 0 {
			cfg.JobTTL = time.Duration(m) * time.Minute
		}
	}
	if os.Getenv("SERVERSIM_CONTINUOUS") == "true" {
		cfg.ContinuousMode = true
	}
	if v := os.Getenv("SERVERSIM_TICK_SECONDS"); v != "" {
		if s, err := strconv.Atoi(v); err == nil {
			if s < 1 {
				s = 1
			}
			cfg.TickInterval = time.Duration(s) * time.Second
		}
	}
	if v := os.Getenv("SERVERSIM_SATURATION_POLICY"); v != "" {
		switch v {
		case SaturationPolicyRetry, SaturationPolicyPassThrough:
			cfg.SaturationPolicy = v
		default:
			log.Printf("config: unknown SERVERSIM_SATURATION_POLICY %q; using default %q", v, cfg.SaturationPolicy)
		}
	}
	if v := os.Getenv("SERVERSIM_LABELS_DIR"); v != "" {
		cfg.LabelsDir = v
	}

	return cfg
}
